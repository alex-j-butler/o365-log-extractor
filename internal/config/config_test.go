package config

import (
	"strings"
	"testing"
	"time"
)

// apiArgs supplies the credentials every api-mode config needs.
var apiArgs = []string{"-tenant-id", "t", "-client-id", "c", "-client-secret", "s"}

func parseAPI(t *testing.T, extra ...string) (*Config, error) {
	t.Helper()
	return Parse(append(append([]string{}, apiArgs...), extra...))
}

func TestParseDefaultsToBothSources(t *testing.T) {
	cfg, err := parseAPI(t)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if !cfg.HasSource(SourceO365) || !cfg.HasSource(SourceIntune) {
		t.Errorf("sources = %v, want both o365 and intune", cfg.Sources)
	}
	if cfg.GraphAPIVersion != "v1.0" {
		t.Errorf("GraphAPIVersion = %q, want v1.0 (the stable version)", cfg.GraphAPIVersion)
	}
}

func TestParseSingleSource(t *testing.T) {
	cfg, err := parseAPI(t, "-sources", "intune")
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if cfg.HasSource(SourceO365) {
		t.Error("o365 should not be enabled")
	}
	if !cfg.HasSource(SourceIntune) {
		t.Error("intune should be enabled")
	}
}

func TestParseRejectsBadSources(t *testing.T) {
	for _, tc := range []struct{ sources, wantErr string }{
		{"o365,entra", "unknown source"},
		{"o365,o365", "listed twice"},
		{"", "at least one feed"},
	} {
		_, err := parseAPI(t, "-sources", tc.sources)
		if err == nil {
			t.Errorf("-sources %q: expected an error", tc.sources)
			continue
		}
		if !strings.Contains(err.Error(), tc.wantErr) {
			t.Errorf("-sources %q: error = %q, want it to mention %q", tc.sources, err, tc.wantErr)
		}
	}
}

// -lookback is capped by the Management Activity API's 7-day retention, but
// only when that feed is actually enabled.
func TestLookbackCapAppliesOnlyToO365(t *testing.T) {
	if _, err := parseAPI(t, "-lookback", "720h"); err == nil {
		t.Error("expected -lookback 720h to be rejected while o365 is enabled")
	}
	if _, err := parseAPI(t, "-sources", "intune", "-lookback", "720h"); err != nil {
		t.Errorf("intune-only -lookback 720h should be allowed: %v", err)
	}
}

func TestIntuneLookbackDefaultsToLookback(t *testing.T) {
	cfg, err := parseAPI(t, "-lookback", "48h")
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if cfg.IntuneLookback != 48*time.Hour {
		t.Errorf("IntuneLookback = %s, want it to track -lookback (48h)", cfg.IntuneLookback)
	}
}

// A long Intune backfill must not drag -lookback past the O365 cap.
func TestIntuneLookbackIsIndependent(t *testing.T) {
	cfg, err := parseAPI(t, "-lookback", "24h", "-intune-lookback", "4380h")
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if cfg.Lookback != 24*time.Hour {
		t.Errorf("Lookback = %s, want 24h", cfg.Lookback)
	}
	if cfg.IntuneLookback != 4380*time.Hour {
		t.Errorf("IntuneLookback = %s, want 4380h", cfg.IntuneLookback)
	}
}

func TestIntuneLookbackCappedAtRetention(t *testing.T) {
	// Beyond roughly two years there is nothing left to read.
	_, err := parseAPI(t, "-intune-lookback", "26280h")
	if err == nil {
		t.Fatal("expected -intune-lookback beyond retention to be rejected")
	}
	if !strings.Contains(err.Error(), "two years") {
		t.Errorf("error = %q, want it to explain the retention limit", err)
	}
}

func TestRejectsUnknownGraphAPIVersion(t *testing.T) {
	if _, err := parseAPI(t, "-graph-api-version", "v2.0"); err == nil {
		t.Error("expected an error for an unknown Graph version")
	}
	for _, version := range []string{"v1.0", "beta", "BETA"} {
		if _, err := parseAPI(t, "-graph-api-version", version); err != nil {
			t.Errorf("-graph-api-version %q: %v", version, err)
		}
	}
}

// Content types only matter to the o365 feed, so an empty list is fine when
// only Intune is enabled.
func TestContentTypesRequiredOnlyForO365(t *testing.T) {
	if _, err := parseAPI(t, "-content-types", ""); err == nil {
		t.Error("expected empty -content-types to be rejected while o365 is enabled")
	}
	if _, err := parseAPI(t, "-sources", "intune", "-content-types", ""); err != nil {
		t.Errorf("empty -content-types should be fine for intune only: %v", err)
	}
}

func TestFileModeStillRequiresFiles(t *testing.T) {
	if _, err := Parse([]string{"-mode", "file"}); err == nil {
		t.Error("expected file mode with no arguments to be rejected")
	}
	if _, err := Parse([]string{"-mode", "file", "export.json"}); err != nil {
		t.Errorf("file mode with a file argument: %v", err)
	}
}

func TestCloudResolvesGraphEndpoint(t *testing.T) {
	cfg, err := parseAPI(t, "-cloud", "gcchigh")
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if cfg.Cloud.GraphURL != "https://graph.microsoft.us" {
		t.Errorf("gcchigh GraphURL = %q", cfg.Cloud.GraphURL)
	}
}
