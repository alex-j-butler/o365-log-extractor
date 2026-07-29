// Package victorialogs writes documents to VictoriaLogs through its
// JSON stream ingestion API (/insert/jsonline).
package victorialogs

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"math"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// Options configures a Client.
type Options struct {
	// URL is the VictoriaLogs base address, e.g. http://localhost:9428.
	URL string
	// Path overrides the ingestion endpoint. Defaults to /insert/jsonline.
	Path string

	// StreamFields are the low-cardinality fields VictoriaLogs uses to
	// group records into log streams.
	StreamFields []string
	// IgnoreFields are dropped at ingestion time by VictoriaLogs.
	IgnoreFields []string
	// ExtraFields are added to every ingested record by VictoriaLogs.
	ExtraFields map[string]string

	// AccountID and ProjectID select a tenant in cluster VictoriaLogs.
	AccountID string
	ProjectID string

	Username    string
	Password    string
	BearerToken string
	Headers     map[string]string

	// Gzip compresses request bodies. Enabled by default via New.
	Gzip bool
	// BatchRecords and BatchBytes bound how much is buffered before a flush.
	BatchRecords int
	BatchBytes   int
	MaxRetries   int
	Timeout      time.Duration

	// DryRun writes newline-delimited JSON to Out instead of shipping it.
	DryRun bool
	Out    io.Writer
	// Debug asks VictoriaLogs to log how it parsed each record without
	// storing it.
	Debug bool

	HTTPClient *http.Client
	Logger     *slog.Logger
}

// Stats reports what a Client has shipped.
type Stats struct {
	Records int64
	Batches int64
	Bytes   int64
}

// Client batches records and POSTs them to VictoriaLogs. It is not safe for
// concurrent use.
type Client struct {
	endpoint string
	opts     Options
	http     *http.Client
	log      *slog.Logger

	buf     bytes.Buffer
	pending int
	stats   Stats
}

// New builds a Client and validates the target URL.
func New(opts Options) (*Client, error) {
	if opts.URL == "" {
		return nil, fmt.Errorf("victorialogs URL is required")
	}
	base, err := url.Parse(strings.TrimSuffix(opts.URL, "/"))
	if err != nil {
		return nil, fmt.Errorf("invalid victorialogs URL: %w", err)
	}
	if base.Scheme == "" || base.Host == "" {
		return nil, fmt.Errorf("invalid victorialogs URL %q: want scheme://host[:port]", opts.URL)
	}

	path := opts.Path
	if path == "" {
		path = "/insert/jsonline"
	}
	if opts.BatchRecords <= 0 {
		opts.BatchRecords = 1000
	}
	if opts.BatchBytes <= 0 {
		opts.BatchBytes = 4 << 20
	}
	if opts.MaxRetries <= 0 {
		opts.MaxRetries = 4
	}
	if opts.Timeout <= 0 {
		opts.Timeout = time.Minute
	}
	if opts.Out == nil {
		opts.Out = io.Discard
	}
	if opts.Logger == nil {
		opts.Logger = slog.Default()
	}
	hc := opts.HTTPClient
	if hc == nil {
		hc = &http.Client{Timeout: opts.Timeout}
	}

	query := url.Values{
		"_time_field": {"_time"},
		"_msg_field":  {"_msg"},
	}
	if len(opts.StreamFields) > 0 {
		query.Set("_stream_fields", strings.Join(opts.StreamFields, ","))
	}
	if len(opts.IgnoreFields) > 0 {
		query.Set("ignore_fields", strings.Join(opts.IgnoreFields, ","))
	}
	if len(opts.ExtraFields) > 0 {
		pairs := make([]string, 0, len(opts.ExtraFields))
		for k, v := range opts.ExtraFields {
			pairs = append(pairs, k+"="+v)
		}
		query.Set("extra_fields", strings.Join(pairs, ","))
	}
	if opts.Debug {
		query.Set("debug", "1")
	}

	base.Path = strings.TrimSuffix(base.Path, "/") + path
	base.RawQuery = query.Encode()

	return &Client{
		endpoint: base.String(),
		opts:     opts,
		http:     hc,
		log:      opts.Logger,
	}, nil
}

// Endpoint returns the fully-qualified ingestion URL, for logging.
func (c *Client) Endpoint() string { return c.endpoint }

// Stats returns cumulative ingestion counters.
func (c *Client) Stats() Stats { return c.stats }

// Add buffers one record, flushing automatically once a batch threshold is
// reached.
func (c *Client) Add(ctx context.Context, record map[string]any) error {
	line, err := json.Marshal(record)
	if err != nil {
		return fmt.Errorf("marshal record: %w", err)
	}
	c.buf.Write(line)
	c.buf.WriteByte('\n')
	c.pending++

	if c.pending >= c.opts.BatchRecords || c.buf.Len() >= c.opts.BatchBytes {
		return c.Flush(ctx)
	}
	return nil
}

// Flush ships any buffered records. It is safe to call when empty.
func (c *Client) Flush(ctx context.Context) error {
	if c.pending == 0 {
		return nil
	}
	body := c.buf.Bytes()
	count := c.pending

	if c.opts.DryRun {
		if _, err := c.opts.Out.Write(body); err != nil {
			return err
		}
	} else if err := c.send(ctx, body); err != nil {
		return err
	}

	c.stats.Records += int64(count)
	c.stats.Batches++
	c.stats.Bytes += int64(len(body))
	c.buf.Reset()
	c.pending = 0
	c.log.Debug("flushed batch", "records", count, "bytes", len(body))
	return nil
}

func (c *Client) send(ctx context.Context, body []byte) error {
	payload := body
	if c.opts.Gzip {
		var compressed bytes.Buffer
		zw := gzip.NewWriter(&compressed)
		if _, err := zw.Write(body); err != nil {
			return fmt.Errorf("gzip: %w", err)
		}
		if err := zw.Close(); err != nil {
			return fmt.Errorf("gzip: %w", err)
		}
		payload = compressed.Bytes()
	}

	var lastErr error
	for attempt := 0; attempt <= c.opts.MaxRetries; attempt++ {
		if attempt > 0 {
			delay := backoff(attempt)
			c.log.Warn("retrying victorialogs write", "attempt", attempt, "delay", delay, "error", lastErr)
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(delay):
			}
		}

		req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.endpoint, bytes.NewReader(payload))
		if err != nil {
			return err
		}
		req.Header.Set("Content-Type", "application/stream+json")
		if c.opts.Gzip {
			req.Header.Set("Content-Encoding", "gzip")
		}
		if c.opts.AccountID != "" {
			req.Header.Set("AccountID", c.opts.AccountID)
		}
		if c.opts.ProjectID != "" {
			req.Header.Set("ProjectID", c.opts.ProjectID)
		}
		if c.opts.BearerToken != "" {
			req.Header.Set("Authorization", "Bearer "+c.opts.BearerToken)
		} else if c.opts.Username != "" || c.opts.Password != "" {
			req.SetBasicAuth(c.opts.Username, c.opts.Password)
		}
		for k, v := range c.opts.Headers {
			req.Header.Set(k, v)
		}

		resp, err := c.http.Do(req)
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			lastErr = err
			continue
		}

		respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<16))
		resp.Body.Close()

		if resp.StatusCode >= 200 && resp.StatusCode < 300 {
			return nil
		}
		lastErr = fmt.Errorf("victorialogs returned %s: %s", resp.Status, strings.TrimSpace(string(respBody)))
		if resp.StatusCode < 500 && resp.StatusCode != http.StatusTooManyRequests {
			return lastErr // a malformed request will not succeed on retry
		}
	}
	return fmt.Errorf("giving up after %d retries: %w", c.opts.MaxRetries, lastErr)
}

func backoff(attempt int) time.Duration {
	d := time.Duration(math.Pow(2, float64(attempt-1))) * time.Second
	return min(d, 30*time.Second)
}

// ParseKeyValues parses repeated "key=value" flag values into a map.
func ParseKeyValues(values []string) (map[string]string, error) {
	if len(values) == 0 {
		return nil, nil
	}
	out := make(map[string]string, len(values))
	for _, v := range values {
		key, value, ok := strings.Cut(v, "=")
		if !ok || strings.TrimSpace(key) == "" {
			return nil, fmt.Errorf("expected key=value, got %q", v)
		}
		out[strings.TrimSpace(key)] = value
	}
	return out, nil
}
