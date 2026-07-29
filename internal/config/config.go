// Package config parses command-line flags and environment variables into
// the settings used by the extractor.
package config

import (
	"flag"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/alex-j-butler/o365-log-extractor/internal/o365"
	"github.com/alex-j-butler/o365-log-extractor/internal/victorialogs"
)

// Mode selects where audit records are read from.
type Mode string

const (
	// ModeAPI pulls from the Office 365 Management Activity API.
	ModeAPI Mode = "api"
	// ModeFile parses audit log exports from disk or stdin.
	ModeFile Mode = "file"
)

// Config is the fully resolved runtime configuration.
type Config struct {
	Mode  Mode
	Files []string

	Cloud         o365.Cloud
	TenantID      string
	ClientID      string
	ClientSecret  string
	PublisherID   string
	ContentTypes  []string
	AutoSubscribe bool
	Lookback      time.Duration
	Overlap       time.Duration
	Follow        bool
	PollInterval  time.Duration
	StateFile     string

	VL victorialogs.Options

	LogLevel string
	LogJSON  bool
}

// stringList collects a repeatable flag into a slice.
type stringList []string

func (s *stringList) String() string { return strings.Join(*s, ",") }

func (s *stringList) Set(v string) error {
	*s = append(*s, v)
	return nil
}

// Parse builds a Config from args. Secrets may be supplied via the
// O365_TENANT_ID, O365_CLIENT_ID and O365_CLIENT_SECRET environment
// variables so they need not appear in the process table.
func Parse(args []string) (*Config, error) {
	fs := flag.NewFlagSet("o365-log-extractor", flag.ContinueOnError)
	fs.Usage = func() {
		out := fs.Output()
		fmt.Fprintf(out, "o365-log-extractor - import Office 365 audit logs into VictoriaLogs\n\n")
		fmt.Fprintf(out, "Usage:\n")
		fmt.Fprintf(out, "  o365-log-extractor -mode api  [flags]\n")
		fmt.Fprintf(out, "  o365-log-extractor -mode file [flags] <file.json|file.csv|file.gz|->\n\n")
		fmt.Fprintf(out, "Flags:\n")
		fs.PrintDefaults()
	}

	var (
		mode          = fs.String("mode", string(ModeAPI), "input mode: api (Management Activity API) or file (audit export)")
		cloudName     = fs.String("cloud", "commercial", "Microsoft cloud: commercial, gcc, gcchigh or dod")
		tenantID      = fs.String("tenant-id", os.Getenv("O365_TENANT_ID"), "Azure AD tenant ID (env O365_TENANT_ID)")
		clientID      = fs.String("client-id", os.Getenv("O365_CLIENT_ID"), "Azure AD application (client) ID (env O365_CLIENT_ID)")
		clientSecret  = fs.String("client-secret", os.Getenv("O365_CLIENT_SECRET"), "Azure AD client secret (env O365_CLIENT_SECRET)")
		publisherID   = fs.String("publisher-id", "", "PublisherIdentifier for API throttling (default: tenant ID)")
		contentTypes  = fs.String("content-types", strings.Join(o365.ContentTypes, ","), "comma-separated content types to pull")
		autoSubscribe = fs.Bool("auto-subscribe", true, "start subscriptions for the requested content types if not already enabled")
		lookback      = fs.Duration("lookback", 24*time.Hour, "how far back to read on first run (max 168h)")
		overlap       = fs.Duration("overlap", 30*time.Minute, "re-query this far behind the cursor to catch late-published blobs")
		follow        = fs.Bool("follow", false, "keep polling for new content instead of exiting after one pass")
		pollInterval  = fs.Duration("poll-interval", 5*time.Minute, "interval between polls when -follow is set")
		stateFile     = fs.String("state-file", "o365-extractor.state.json", "path to the ingestion state file (empty disables state)")

		vlURL          = fs.String("vl-url", "http://localhost:9428", "VictoriaLogs base URL")
		vlPath         = fs.String("vl-path", "/insert/jsonline", "VictoriaLogs ingestion path")
		vlStreamFields = fs.String("vl-stream-fields", "Workload,RecordTypeName", "comma-separated low-cardinality fields used as log stream labels")
		vlIgnoreFields = fs.String("vl-ignore-fields", "", "comma-separated fields for VictoriaLogs to drop")
		vlAccountID    = fs.String("vl-account-id", "", "AccountID header for cluster VictoriaLogs")
		vlProjectID    = fs.String("vl-project-id", "", "ProjectID header for cluster VictoriaLogs")
		vlUsername     = fs.String("vl-username", os.Getenv("VL_USERNAME"), "basic auth username (env VL_USERNAME)")
		vlPassword     = fs.String("vl-password", os.Getenv("VL_PASSWORD"), "basic auth password (env VL_PASSWORD)")
		vlBearer       = fs.String("vl-bearer-token", os.Getenv("VL_BEARER_TOKEN"), "bearer token (env VL_BEARER_TOKEN)")
		vlGzip         = fs.Bool("vl-gzip", true, "gzip-compress ingestion requests")
		vlDebug        = fs.Bool("vl-debug", false, "ask VictoriaLogs to parse and log records without storing them")
		batchRecords   = fs.Int("batch-records", 1000, "flush after this many records")
		batchBytes     = fs.Int("batch-bytes", 4<<20, "flush after this many buffered bytes")
		maxRetries     = fs.Int("max-retries", 4, "retry attempts for failed HTTP requests")
		timeout        = fs.Duration("timeout", time.Minute, "per-request HTTP timeout")
		dryRun         = fs.Bool("dry-run", false, "print records as JSON lines to stdout instead of writing to VictoriaLogs")

		logLevel = fs.String("log-level", "info", "log level: debug, info, warn or error")
		logJSON  = fs.Bool("log-json", false, "emit structured JSON logs")
	)
	var (
		vlHeaders    stringList
		vlExtraField stringList
	)
	fs.Var(&vlHeaders, "vl-header", "extra HTTP header as key=value (repeatable)")
	fs.Var(&vlExtraField, "vl-extra-field", "field added to every record as key=value (repeatable)")

	if err := fs.Parse(args); err != nil {
		return nil, err
	}

	cloud, err := o365.LookupCloud(*cloudName)
	if err != nil {
		return nil, err
	}
	headers, err := victorialogs.ParseKeyValues(vlHeaders)
	if err != nil {
		return nil, fmt.Errorf("-vl-header: %w", err)
	}
	extra, err := victorialogs.ParseKeyValues(vlExtraField)
	if err != nil {
		return nil, fmt.Errorf("-vl-extra-field: %w", err)
	}

	cfg := &Config{
		Mode:          Mode(strings.ToLower(*mode)),
		Files:         fs.Args(),
		Cloud:         cloud,
		TenantID:      strings.TrimSpace(*tenantID),
		ClientID:      strings.TrimSpace(*clientID),
		ClientSecret:  *clientSecret,
		PublisherID:   strings.TrimSpace(*publisherID),
		ContentTypes:  splitList(*contentTypes),
		AutoSubscribe: *autoSubscribe,
		Lookback:      *lookback,
		Overlap:       *overlap,
		Follow:        *follow,
		PollInterval:  *pollInterval,
		StateFile:     *stateFile,
		LogLevel:      *logLevel,
		LogJSON:       *logJSON,
		VL: victorialogs.Options{
			URL:          *vlURL,
			Path:         *vlPath,
			StreamFields: splitList(*vlStreamFields),
			IgnoreFields: splitList(*vlIgnoreFields),
			ExtraFields:  extra,
			AccountID:    *vlAccountID,
			ProjectID:    *vlProjectID,
			Username:     *vlUsername,
			Password:     *vlPassword,
			BearerToken:  *vlBearer,
			Headers:      headers,
			Gzip:         *vlGzip,
			BatchRecords: *batchRecords,
			BatchBytes:   *batchBytes,
			MaxRetries:   *maxRetries,
			Timeout:      *timeout,
			DryRun:       *dryRun,
			Out:          os.Stdout,
			Debug:        *vlDebug,
		},
	}

	if err := cfg.validate(); err != nil {
		return nil, err
	}
	return cfg, nil
}

func (c *Config) validate() error {
	switch c.Mode {
	case ModeAPI:
		var missing []string
		if c.TenantID == "" {
			missing = append(missing, "-tenant-id")
		}
		if c.ClientID == "" {
			missing = append(missing, "-client-id")
		}
		if c.ClientSecret == "" {
			missing = append(missing, "-client-secret")
		}
		if len(missing) > 0 {
			return fmt.Errorf("api mode requires %s", strings.Join(missing, ", "))
		}
		if len(c.ContentTypes) == 0 {
			return fmt.Errorf("-content-types must list at least one content type")
		}
		if c.Lookback <= 0 {
			return fmt.Errorf("-lookback must be positive")
		}
		if c.Lookback > 7*24*time.Hour {
			return fmt.Errorf("-lookback cannot exceed 168h: the API retains content for 7 days")
		}
		if c.Overlap < 0 {
			return fmt.Errorf("-overlap cannot be negative")
		}
		if c.Follow && c.PollInterval <= 0 {
			return fmt.Errorf("-poll-interval must be positive when -follow is set")
		}
	case ModeFile:
		if len(c.Files) == 0 {
			return fmt.Errorf("file mode requires at least one file argument (use - for stdin)")
		}
	default:
		return fmt.Errorf("unknown -mode %q (want api or file)", c.Mode)
	}
	return nil
}

func splitList(v string) []string {
	parts := strings.Split(v, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}
