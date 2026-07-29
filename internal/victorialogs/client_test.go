package victorialogs

import (
	"compress/gzip"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestEndpointIncludesIngestionParams(t *testing.T) {
	c, err := New(Options{
		URL:          "http://localhost:9428/",
		StreamFields: []string{"Workload", "RecordTypeName"},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	got := c.Endpoint()
	for _, want := range []string{
		"http://localhost:9428/insert/jsonline?",
		"_time_field=_time",
		"_msg_field=_msg",
		"_stream_fields=Workload%2CRecordTypeName",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("endpoint %q missing %q", got, want)
		}
	}
}

func TestNewRejectsBadURL(t *testing.T) {
	if _, err := New(Options{URL: "localhost:9428"}); err == nil {
		t.Error("expected an error for a URL without a scheme")
	}
	if _, err := New(Options{}); err == nil {
		t.Error("expected an error for an empty URL")
	}
}

func TestFlushSendsGzippedJSONLines(t *testing.T) {
	var body string
	var encoding string

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		encoding = r.Header.Get("Content-Encoding")
		zr, err := gzip.NewReader(r.Body)
		if err != nil {
			t.Errorf("gzip reader: %v", err)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		defer zr.Close()
		raw, err := io.ReadAll(zr)
		if err != nil {
			t.Errorf("read body: %v", err)
		}
		body = string(raw)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	c, err := New(Options{URL: srv.URL, Gzip: true})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	ctx := context.Background()
	for _, op := range []string{"UserLoggedIn", "FileAccessed"} {
		if err := c.Add(ctx, map[string]any{"_msg": op, "_time": "2026-07-28T04:15:22Z"}); err != nil {
			t.Fatalf("Add: %v", err)
		}
	}
	if err := c.Flush(ctx); err != nil {
		t.Fatalf("Flush: %v", err)
	}

	if encoding != "gzip" {
		t.Errorf("Content-Encoding = %q, want gzip", encoding)
	}
	if lines := strings.Count(body, "\n"); lines != 2 {
		t.Errorf("body has %d lines, want 2: %q", lines, body)
	}
	if stats := c.Stats(); stats.Records != 2 || stats.Batches != 1 {
		t.Errorf("stats = %+v, want 2 records in 1 batch", stats)
	}
}

func TestAddFlushesOnBatchSize(t *testing.T) {
	var requests int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		io.Copy(io.Discard, r.Body)
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()

	c, err := New(Options{URL: srv.URL, BatchRecords: 2})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	ctx := context.Background()
	for i := 0; i < 5; i++ {
		if err := c.Add(ctx, map[string]any{"_msg": "x"}); err != nil {
			t.Fatalf("Add: %v", err)
		}
	}
	if requests != 2 {
		t.Errorf("sent %d requests, want 2 (5 records at a batch size of 2)", requests)
	}
	if err := c.Flush(ctx); err != nil {
		t.Fatalf("Flush: %v", err)
	}
	if requests != 3 {
		t.Errorf("sent %d requests after final flush, want 3", requests)
	}
}

func TestSendDoesNotRetryClientErrors(t *testing.T) {
	var attempts int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		attempts++
		http.Error(w, "cannot parse json", http.StatusBadRequest)
	}))
	defer srv.Close()

	c, err := New(Options{URL: srv.URL})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if err := c.Add(context.Background(), map[string]any{"_msg": "x"}); err != nil {
		t.Fatalf("Add: %v", err)
	}
	if err := c.Flush(context.Background()); err == nil {
		t.Fatal("expected an error from a 400 response")
	}
	if attempts != 1 {
		t.Errorf("made %d attempts, want 1 (400 is not retryable)", attempts)
	}
}

func TestDryRunWritesToOut(t *testing.T) {
	var out strings.Builder
	c, err := New(Options{URL: "http://localhost:9428", DryRun: true, Out: &out})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if err := c.Add(context.Background(), map[string]any{"_msg": "UserLoggedIn"}); err != nil {
		t.Fatalf("Add: %v", err)
	}
	if err := c.Flush(context.Background()); err != nil {
		t.Fatalf("Flush: %v", err)
	}
	if !strings.Contains(out.String(), `"_msg":"UserLoggedIn"`) {
		t.Errorf("dry-run output = %q", out.String())
	}
}

func TestParseKeyValues(t *testing.T) {
	got, err := ParseKeyValues([]string{"X-Scope-OrgID=tenant-a", "X-Token=a=b"})
	if err != nil {
		t.Fatalf("ParseKeyValues: %v", err)
	}
	if got["X-Scope-OrgID"] != "tenant-a" {
		t.Errorf("got %v", got)
	}
	if got["X-Token"] != "a=b" {
		t.Errorf("value with '=' should be preserved, got %q", got["X-Token"])
	}
	if _, err := ParseKeyValues([]string{"novalue"}); err == nil {
		t.Error("expected an error for a value without '='")
	}
}
