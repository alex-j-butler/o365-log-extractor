package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/alex-j-butler/o365-log-extractor/internal/config"
	"github.com/alex-j-butler/o365-log-extractor/internal/intune"
	"github.com/alex-j-butler/o365-log-extractor/internal/msapi"
	"github.com/alex-j-butler/o365-log-extractor/internal/state"
	"github.com/alex-j-butler/o365-log-extractor/internal/victorialogs"
)

func auditEvent(id, activityDateTime string) map[string]any {
	return map[string]any{
		"id":               id,
		"activity":         "Patch DeviceConfiguration",
		"activityDateTime": activityDateTime,
		"activityResult":   "Success",
		"category":         "DeviceConfiguration",
		"componentName":    "DeviceConfiguration",
		"actor":            map[string]any{"userPrincipalName": "admin@example.com"},
	}
}

// graphStub serves tokens plus whatever audit events the test queues up.
func graphStub(t *testing.T, pages *[][]map[string]any) *httptest.Server {
	t.Helper()

	var calls int
	mux := http.NewServeMux()
	mux.HandleFunc("/tenant-id/oauth2/v2.0/token", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"access_token":"test-token","expires_in":3600}`)
	})
	mux.HandleFunc("/v1.0/deviceManagement/auditEvents", func(w http.ResponseWriter, r *http.Request) {
		var events []map[string]any
		if calls < len(*pages) {
			events = (*pages)[calls]
		}
		calls++
		json.NewEncoder(w).Encode(map[string]any{"value": events})
	})

	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

// sinkStub captures every JSON line the extractor ships.
func sinkStub(t *testing.T) (*victorialogs.Client, *[]map[string]any) {
	t.Helper()

	var got []map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		for _, line := range strings.Split(strings.TrimSpace(string(body)), "\n") {
			if line == "" {
				continue
			}
			var record map[string]any
			if err := json.Unmarshal([]byte(line), &record); err != nil {
				t.Errorf("sink received invalid JSON line %q: %v", line, err)
				continue
			}
			got = append(got, record)
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	t.Cleanup(srv.Close)

	sink, err := victorialogs.New(victorialogs.Options{URL: srv.URL, Gzip: false})
	if err != nil {
		t.Fatalf("victorialogs.New: %v", err)
	}
	return sink, &got
}

func testConfig(srv *httptest.Server) *config.Config {
	return &config.Config{
		Cloud:          msapi.Cloud{LoginURL: srv.URL, GraphURL: srv.URL},
		TenantID:       "tenant-id",
		IntuneLookback: 24 * time.Hour,
		Overlap:        30 * time.Minute,
	}
}

func newIntuneClient(srv *httptest.Server) *intune.Client {
	return intune.New(intune.Options{
		Cloud:    msapi.Cloud{LoginURL: srv.URL, GraphURL: srv.URL},
		TenantID: "tenant-id",
	})
}

// The whole Intune path end to end: Graph -> normalise -> VictoriaLogs.
func TestPollIntuneIngestsNormalisedRecords(t *testing.T) {
	pages := [][]map[string]any{{
		auditEvent("event-1", "2026-07-31T09:00:00Z"),
		auditEvent("event-2", "2026-07-31T09:05:00Z"),
	}}
	srv := graphStub(t, &pages)
	sink, got := sinkStub(t)
	st := state.New("")

	now := time.Date(2026, 7, 31, 10, 0, 0, 0, time.UTC)
	err := pollIntune(context.Background(), testConfig(srv), newIntuneClient(srv), sink, st,
		slog.New(slog.DiscardHandler), now)
	if err != nil {
		t.Fatalf("pollIntune: %v", err)
	}

	if len(*got) != 2 {
		t.Fatalf("ingested %d records, want 2", len(*got))
	}
	record := (*got)[0]

	checks := map[string]any{
		"_time":          "2026-07-31T09:00:00Z",
		"_msg":           "Patch DeviceConfiguration user=admin@example.com workload=MicrosoftIntune result=Success component=DeviceConfiguration",
		"Workload":       "MicrosoftIntune",
		"RecordTypeName": "IntuneDeviceConfiguration",
		"UserId":         "admin@example.com",
		"source":         "intune-graph-api",
		"TenantIdHint":   "tenant-id",
	}
	for field, want := range checks {
		if got := record[field]; got != want {
			t.Errorf("%s = %v, want %v", field, got, want)
		}
	}

	// The cursor must advance so the next poll resumes rather than re-reading.
	cursor, ok := st.Cursor(intuneCursorKey)
	if !ok {
		t.Fatal("cursor was not set")
	}
	if !cursor.Equal(now) {
		t.Errorf("cursor = %s, want %s", cursor, now)
	}
}

// The overlap window deliberately re-reads events; IDs must suppress them.
func TestPollIntuneDeduplicatesAcrossPolls(t *testing.T) {
	pages := [][]map[string]any{
		{
			auditEvent("event-1", "2026-07-31T09:00:00Z"),
			auditEvent("event-2", "2026-07-31T09:05:00Z"),
		},
		{
			auditEvent("event-2", "2026-07-31T09:05:00Z"), // re-delivered
			auditEvent("event-3", "2026-07-31T10:30:00Z"), // new
		},
	}
	srv := graphStub(t, &pages)
	sink, got := sinkStub(t)
	st := state.New("")
	cfg := testConfig(srv)
	log := slog.New(slog.DiscardHandler)

	first := time.Date(2026, 7, 31, 10, 0, 0, 0, time.UTC)
	if err := pollIntune(context.Background(), cfg, newIntuneClient(srv), sink, st, log, first); err != nil {
		t.Fatalf("first poll: %v", err)
	}
	second := first.Add(time.Hour)
	if err := pollIntune(context.Background(), cfg, newIntuneClient(srv), sink, st, log, second); err != nil {
		t.Fatalf("second poll: %v", err)
	}

	if len(*got) != 3 {
		t.Fatalf("ingested %d records, want 3 (event-2 must not be duplicated)", len(*got))
	}
	ids := map[string]int{}
	for _, record := range *got {
		id, _ := record["id"].(string)
		ids[id]++
	}
	for id, count := range ids {
		if count != 1 {
			t.Errorf("%s ingested %d times, want 1", id, count)
		}
	}
	if len(ids) != 3 {
		t.Errorf("ingested ids = %v, want event-1..3", ids)
	}
}

// A failing feed must not advance its cursor, or the window would be lost.
func TestPollIntuneLeavesCursorOnFailure(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/tenant-id/oauth2/v2.0/token", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"access_token":"t","expires_in":3600}`)
	})
	mux.HandleFunc("/v1.0/deviceManagement/auditEvents", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		fmt.Fprint(w, `{"error":{"code":"Forbidden","message":"Application is not authorized"}}`)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	sink, _ := sinkStub(t)
	st := state.New("")

	err := pollIntune(context.Background(), testConfig(srv), newIntuneClient(srv), sink, st,
		slog.New(slog.DiscardHandler), time.Now().UTC())
	if err == nil {
		t.Fatal("expected an error from a 403 response")
	}
	if _, ok := st.Cursor(intuneCursorKey); ok {
		t.Error("cursor advanced despite the poll failing")
	}
	// The operator needs to be told which permission is missing.
	if hint := intuneErrorHint(err); !strings.Contains(hint, "DeviceManagementApps.Read.All") {
		t.Errorf("hint = %q, want it to name the required permission", hint)
	}
}
