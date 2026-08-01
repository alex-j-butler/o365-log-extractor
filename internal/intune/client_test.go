package intune

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/alex-j-butler/o365-log-extractor/internal/msapi"
)

// newTestClient points both the token endpoint and the Graph endpoint at a
// test server, so no real credentials are involved.
func newTestClient(t *testing.T, handler http.Handler) (*Client, *httptest.Server) {
	t.Helper()

	mux := http.NewServeMux()
	mux.HandleFunc("/tenant-id/oauth2/v2.0/token", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"access_token":"test-token","expires_in":3600,"token_type":"Bearer"}`)
	})
	mux.Handle("/", handler)

	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	client := New(Options{
		Cloud: msapi.Cloud{
			Name:     "test",
			LoginURL: srv.URL,
			GraphURL: srv.URL,
		},
		TenantID:     "tenant-id",
		ClientID:     "client-id",
		ClientSecret: "secret",
	})
	return client, srv
}

func collectEvents(t *testing.T, c *Client, start, end time.Time) []map[string]any {
	t.Helper()
	var got []map[string]any
	if err := c.ListAuditEvents(context.Background(), start, end, func(raw map[string]any) error {
		got = append(got, raw)
		return nil
	}); err != nil {
		t.Fatalf("ListAuditEvents: %v", err)
	}
	return got
}

func TestListAuditEventsBuildsFilteredRequest(t *testing.T) {
	var gotPath, gotFilter, gotTop, gotAuth string

	client, _ := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotFilter = r.URL.Query().Get("$filter")
		gotTop = r.URL.Query().Get("$top")
		gotAuth = r.Header.Get("Authorization")
		fmt.Fprint(w, `{"value":[{"id":"a"}]}`)
	}))

	start := time.Date(2026, 7, 30, 0, 0, 0, 0, time.UTC)
	end := time.Date(2026, 7, 31, 0, 0, 0, 0, time.UTC)
	if got := collectEvents(t, client, start, end); len(got) != 1 {
		t.Fatalf("got %d events, want 1", len(got))
	}

	if want := "/v1.0/deviceManagement/auditEvents"; gotPath != want {
		t.Errorf("path = %q, want %q", gotPath, want)
	}
	// Half-open window: gt start, le end.
	want := "activityDateTime gt 2026-07-30T00:00:00Z and activityDateTime le 2026-07-31T00:00:00Z"
	if gotFilter != want {
		t.Errorf("$filter = %q, want %q", gotFilter, want)
	}
	if gotTop != "1000" {
		t.Errorf("$top = %q, want 1000", gotTop)
	}
	if gotAuth != "Bearer test-token" {
		t.Errorf("Authorization = %q", gotAuth)
	}
}

// Graph is happier with %20 than '+' in an OData $filter, so the raw query
// string must not contain '+' separators.
func TestListAuditEventsEncodesSpacesAsPercent20(t *testing.T) {
	var rawQuery string
	client, _ := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rawQuery = r.URL.RawQuery
		fmt.Fprint(w, `{"value":[]}`)
	}))

	collectEvents(t, client, time.Now().Add(-time.Hour), time.Now())

	if strings.Contains(rawQuery, "+") {
		t.Errorf("raw query encodes spaces as '+': %q", rawQuery)
	}
	if !strings.Contains(rawQuery, "%20") {
		t.Errorf("raw query should percent-encode spaces: %q", rawQuery)
	}
}

func TestListAuditEventsFollowsNextLink(t *testing.T) {
	var srv *httptest.Server
	var pages int

	client, srv := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		pages++
		switch r.URL.Query().Get("page") {
		case "":
			// Graph returns an absolute URL that already carries the filter
			// and skip token; it must be followed verbatim.
			next := srv.URL + "/v1.0/deviceManagement/auditEvents?page=2&$skiptoken=abc"
			resp := map[string]any{
				"value":           []map[string]any{{"id": "a"}, {"id": "b"}},
				"@odata.nextLink": next,
			}
			json.NewEncoder(w).Encode(resp)
		case "2":
			if r.URL.Query().Get("$skiptoken") != "abc" {
				t.Errorf("skiptoken not preserved: %q", r.URL.RawQuery)
			}
			fmt.Fprint(w, `{"value":[{"id":"c"}]}`)
		}
	}))

	got := collectEvents(t, client, time.Now().Add(-time.Hour), time.Now())
	if len(got) != 3 {
		t.Fatalf("got %d events across pages, want 3", len(got))
	}
	if pages != 2 {
		t.Errorf("fetched %d pages, want 2", pages)
	}
	if got[2]["id"] != "c" {
		t.Errorf("last event = %v", got[2])
	}
}

func TestListAuditEventsSkipsEmptyWindow(t *testing.T) {
	var called bool
	client, _ := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
		fmt.Fprint(w, `{"value":[]}`)
	}))

	now := time.Now()
	// start == end, and start after end: neither should reach the server.
	collectEvents(t, client, now, now)
	collectEvents(t, client, now, now.Add(-time.Hour))

	if called {
		t.Error("expected no request for an empty time window")
	}
}

func TestListAuditEventsSurfacesForbidden(t *testing.T) {
	client, _ := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		fmt.Fprint(w, `{"error":{"code":"Forbidden","message":"Application is not authorized"}}`)
	}))

	err := client.ListAuditEvents(context.Background(), time.Now().Add(-time.Hour), time.Now(),
		func(map[string]any) error { return nil })
	if err == nil {
		t.Fatal("expected an error for a 403 response")
	}
	// main.go relies on this to print an actionable permissions hint.
	if !msapi.IsForbidden(err) {
		t.Errorf("IsForbidden(%v) = false, want true", err)
	}
}

func TestListAuditEventsPropagatesCallbackError(t *testing.T) {
	client, _ := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"value":[{"id":"a"},{"id":"b"}]}`)
	}))

	wantErr := fmt.Errorf("sink failed")
	seen := 0
	err := client.ListAuditEvents(context.Background(), time.Now().Add(-time.Hour), time.Now(),
		func(map[string]any) error {
			seen++
			return wantErr
		})
	if err != wantErr {
		t.Errorf("error = %v, want %v", err, wantErr)
	}
	if seen != 1 {
		t.Errorf("callback ran %d times, want 1 (should abort immediately)", seen)
	}
}

func TestBetaAPIVersion(t *testing.T) {
	var gotPath string
	mux := http.NewServeMux()
	mux.HandleFunc("/tenant-id/oauth2/v2.0/token", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"access_token":"t","expires_in":3600}`)
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		fmt.Fprint(w, `{"value":[]}`)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	client := New(Options{
		Cloud:      msapi.Cloud{LoginURL: srv.URL, GraphURL: srv.URL},
		TenantID:   "tenant-id",
		APIVersion: "beta",
	})
	collectEvents(t, client, time.Now().Add(-time.Hour), time.Now())

	if want := "/beta/deviceManagement/auditEvents"; gotPath != want {
		t.Errorf("path = %q, want %q", gotPath, want)
	}
}

func TestEncodeQueryKeepsEncodedPlus(t *testing.T) {
	// A literal '+' in a value must stay %2B, not become a space.
	got := encodeQuery(url.Values{"a": {"x + y"}})
	if want := "a=x%20%2B%20y"; got != want {
		t.Errorf("encodeQuery = %q, want %q", got, want)
	}
}
