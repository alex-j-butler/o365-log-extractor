package msapi

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// newTestClient wires a Client to a test server that also serves tokens.
func newTestClient(t *testing.T, handler http.HandlerFunc) (*Client, *httptest.Server) {
	t.Helper()

	mux := http.NewServeMux()
	mux.HandleFunc("/tenant/oauth2/v2.0/token", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"access_token":"test-token","expires_in":3600}`)
	})
	mux.HandleFunc("/api/", handler)

	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	tokens := NewTokenSource(srv.URL, "tenant", "client", "secret", srv.URL, nil)
	return NewClient(tokens, nil, nil, 3), srv
}

func TestDoRetriesThrottledRequests(t *testing.T) {
	var attempts int
	client, srv := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		attempts++
		if attempts == 1 {
			w.Header().Set("Retry-After", "1")
			w.WriteHeader(http.StatusTooManyRequests)
			fmt.Fprint(w, `{"error":{"code":"AF429","message":"throttled"}}`)
			return
		}
		fmt.Fprint(w, `{"ok":true}`)
	})

	start := time.Now()
	resp, err := client.Do(context.Background(), http.MethodGet, srv.URL+"/api/thing")
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	resp.Body.Close()

	if attempts != 2 {
		t.Errorf("made %d attempts, want 2", attempts)
	}
	// The server asked for a one second pause; honouring it is the point.
	if elapsed := time.Since(start); elapsed < time.Second {
		t.Errorf("returned after %s, want at least 1s (Retry-After ignored)", elapsed)
	}
}

func TestDoDoesNotRetryClientErrors(t *testing.T) {
	var attempts int
	client, srv := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		attempts++
		w.WriteHeader(http.StatusBadRequest)
		fmt.Fprint(w, `{"error":{"code":"AF20023","message":"bad content type"}}`)
	})

	_, err := client.Do(context.Background(), http.MethodGet, srv.URL+"/api/thing")
	if err == nil {
		t.Fatal("expected an error for a 400 response")
	}
	if attempts != 1 {
		t.Errorf("made %d attempts, want 1 (400 is not retryable)", attempts)
	}

	var apiErr *APIError
	if !errors.As(err, &apiErr) {
		t.Fatalf("error %v is not an *APIError", err)
	}
	if apiErr.Code != "AF20023" || apiErr.Message != "bad content type" {
		t.Errorf("decoded error = %+v, want the envelope's code and message", apiErr)
	}
}

func TestDoGivesUpAfterMaxRetries(t *testing.T) {
	var attempts int
	mux := http.NewServeMux()
	mux.HandleFunc("/tenant/oauth2/v2.0/token", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"access_token":"t","expires_in":3600}`)
	})
	mux.HandleFunc("/api/", func(w http.ResponseWriter, r *http.Request) {
		attempts++
		w.Header().Set("Retry-After", "0")
		w.WriteHeader(http.StatusServiceUnavailable)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	// One retry keeps the exponential backoff to a single second.
	client := NewClient(NewTokenSource(srv.URL, "tenant", "c", "s", srv.URL, nil), nil, nil, 1)
	if _, err := client.Do(context.Background(), http.MethodGet, srv.URL+"/api/x"); err == nil {
		t.Fatal("expected an error after exhausting retries")
	}
	if attempts != 2 {
		t.Errorf("made %d attempts, want 2 (initial + 1 retry)", attempts)
	}
}

func TestIsForbidden(t *testing.T) {
	for _, tc := range []struct {
		status int
		want   bool
	}{
		{http.StatusForbidden, true},
		{http.StatusUnauthorized, true},
		{http.StatusBadRequest, false},
		{http.StatusInternalServerError, false},
	} {
		err := error(&APIError{StatusCode: tc.status})
		if got := IsForbidden(fmt.Errorf("wrapped: %w", err)); got != tc.want {
			t.Errorf("IsForbidden(%d) = %v, want %v", tc.status, got, tc.want)
		}
	}
	if IsForbidden(errors.New("plain")) {
		t.Error("IsForbidden(plain error) = true, want false")
	}
}

func TestTokenIsCachedAcrossRequests(t *testing.T) {
	var tokenRequests int
	mux := http.NewServeMux()
	mux.HandleFunc("/tenant/oauth2/v2.0/token", func(w http.ResponseWriter, r *http.Request) {
		tokenRequests++
		fmt.Fprint(w, `{"access_token":"t","expires_in":3600}`)
	})
	mux.HandleFunc("/api/", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{}`)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	client := NewClient(NewTokenSource(srv.URL, "tenant", "c", "s", srv.URL, nil), nil, nil, 1)
	for range 3 {
		resp, err := client.Do(context.Background(), http.MethodGet, srv.URL+"/api/x")
		if err != nil {
			t.Fatalf("Do: %v", err)
		}
		resp.Body.Close()
	}
	if tokenRequests != 1 {
		t.Errorf("fetched %d tokens for 3 requests, want 1", tokenRequests)
	}
}

func TestTokenSourceReportsAuthFailure(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		fmt.Fprint(w, `{"error":"invalid_client","error_description":"bad secret"}`)
	}))
	defer srv.Close()

	_, err := NewTokenSource(srv.URL, "tenant", "c", "bad", srv.URL, nil).Token(context.Background())
	if err == nil {
		t.Fatal("expected an error for a rejected client secret")
	}
	// The scope is the fastest way to tell which API's credentials failed.
	if !strings.Contains(err.Error(), "/.default") {
		t.Errorf("error %q should name the scope", err)
	}
}

func TestLookupCloudResolvesPerCloudEndpoints(t *testing.T) {
	commercial, err := LookupCloud("commercial")
	if err != nil {
		t.Fatalf("LookupCloud: %v", err)
	}
	if commercial.GraphURL != "https://graph.microsoft.com" {
		t.Errorf("commercial GraphURL = %q", commercial.GraphURL)
	}

	// Sovereign clouds host Graph and the Management API on different hosts.
	dod, err := LookupCloud("DoD")
	if err != nil {
		t.Fatalf("LookupCloud: %v", err)
	}
	if dod.GraphURL != "https://dod-graph.microsoft.us" {
		t.Errorf("dod GraphURL = %q", dod.GraphURL)
	}
	if dod.ManagementURL != "https://manage.protection.apps.mil" {
		t.Errorf("dod ManagementURL = %q", dod.ManagementURL)
	}

	if _, err := LookupCloud("azure"); err == nil {
		t.Error("expected an error for an unknown cloud")
	}
}
