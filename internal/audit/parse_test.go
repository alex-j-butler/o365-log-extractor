package audit

import (
	"bytes"
	"compress/gzip"
	"strings"
	"testing"
)

func collect(t *testing.T, input string) []map[string]any {
	t.Helper()
	var got []map[string]any
	if err := ReadStream(strings.NewReader(input), func(raw map[string]any) error {
		got = append(got, raw)
		return nil
	}); err != nil {
		t.Fatalf("ReadStream: %v", err)
	}
	return got
}

func TestReadStreamJSONArray(t *testing.T) {
	got := collect(t, `[{"Operation":"UserLoggedIn"},{"Operation":"FileAccessed"}]`)
	if len(got) != 2 {
		t.Fatalf("parsed %d records, want 2", len(got))
	}
	if got[1]["Operation"] != "FileAccessed" {
		t.Errorf("second record = %v", got[1])
	}
}

func TestReadStreamJSONLines(t *testing.T) {
	got := collect(t, "{\"Operation\":\"A\"}\n{\"Operation\":\"B\"}\n")
	if len(got) != 2 {
		t.Fatalf("parsed %d records, want 2", len(got))
	}
}

func TestReadStreamEmpty(t *testing.T) {
	if got := collect(t, "   \n"); len(got) != 0 {
		t.Fatalf("parsed %d records, want 0", len(got))
	}
}

func TestReadStreamCSVWithAuditData(t *testing.T) {
	csv := "CreationDate,UserIds,Operations,AuditData\n" +
		`2026-07-28T04:15:22,alex@example.com,UserLoggedIn,"{""Operation"":""UserLoggedIn"",""Workload"":""AzureActiveDirectory""}"` + "\n"

	got := collect(t, csv)
	if len(got) != 1 {
		t.Fatalf("parsed %d records, want 1", len(got))
	}
	if got[0]["Workload"] != "AzureActiveDirectory" {
		t.Errorf("Workload = %v, want AzureActiveDirectory", got[0]["Workload"])
	}
	if got[0]["UserIds"] != "alex@example.com" {
		t.Errorf("outer column not preserved: %v", got[0])
	}
}

func TestReadStreamCSVWithoutAuditData(t *testing.T) {
	got := collect(t, "CreationTime,Operation\n2026-07-28T04:15:22,UserLoggedIn\n")
	if len(got) != 1 {
		t.Fatalf("parsed %d records, want 1", len(got))
	}
	if got[0]["Operation"] != "UserLoggedIn" {
		t.Errorf("Operation = %v", got[0]["Operation"])
	}
}

func TestReadStreamGzip(t *testing.T) {
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	if _, err := zw.Write([]byte(`[{"Operation":"UserLoggedIn"}]`)); err != nil {
		t.Fatalf("write gzip: %v", err)
	}
	if err := zw.Close(); err != nil {
		t.Fatalf("close gzip: %v", err)
	}

	var got []map[string]any
	if err := ReadStream(&buf, func(raw map[string]any) error {
		got = append(got, raw)
		return nil
	}); err != nil {
		t.Fatalf("ReadStream: %v", err)
	}
	if len(got) != 1 || got[0]["Operation"] != "UserLoggedIn" {
		t.Errorf("gzip records = %v", got)
	}
}

func TestReadStreamSkipsBOM(t *testing.T) {
	got := collect(t, "\ufeff[{\"Operation\":\"UserLoggedIn\"}]")
	if len(got) != 1 {
		t.Fatalf("parsed %d records, want 1", len(got))
	}
}
