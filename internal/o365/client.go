package o365

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// ContentTypes are the feeds published by the Management Activity API.
var ContentTypes = []string{
	"Audit.AzureActiveDirectory",
	"Audit.Exchange",
	"Audit.SharePoint",
	"Audit.General",
	"DLP.All",
}

const (
	// maxQueryWindow is the largest time range the content endpoint accepts
	// in a single request.
	maxQueryWindow = 24 * time.Hour
	// maxRetention is how far back content blobs remain available.
	maxRetention = 7 * 24 * time.Hour
	// apiTimeLayout is the timestamp format the API expects and returns.
	apiTimeLayout = "2006-01-02T15:04:05"
)

// Client talks to the Management Activity API for a single tenant.
type Client struct {
	tokens      *TokenSource
	baseURL     string
	tenantID    string
	publisherID string
	http        *http.Client
	log         *slog.Logger
	maxRetries  int
}

// Options configures a Client.
type Options struct {
	Cloud        Cloud
	TenantID     string
	ClientID     string
	ClientSecret string
	// PublisherID scopes API throttling to your own application. It
	// defaults to the tenant ID, per Microsoft's guidance.
	PublisherID string
	HTTPClient  *http.Client
	Logger      *slog.Logger
	MaxRetries  int
}

// New builds a Client from Options.
func New(opts Options) *Client {
	hc := opts.HTTPClient
	if hc == nil {
		hc = &http.Client{Timeout: 2 * time.Minute}
	}
	logger := opts.Logger
	if logger == nil {
		logger = slog.Default()
	}
	publisher := opts.PublisherID
	if publisher == "" {
		publisher = opts.TenantID
	}
	retries := opts.MaxRetries
	if retries <= 0 {
		retries = 4
	}
	return &Client{
		tokens:      NewTokenSource(opts.Cloud, opts.TenantID, opts.ClientID, opts.ClientSecret, hc),
		baseURL:     strings.TrimSuffix(opts.Cloud.APIURL, "/"),
		tenantID:    opts.TenantID,
		publisherID: publisher,
		http:        hc,
		log:         logger,
		maxRetries:  retries,
	}
}

// Subscription describes one content-type feed and whether it is enabled.
type Subscription struct {
	ContentType string `json:"contentType"`
	Status      string `json:"status"`
}

// ContentBlob points at a batch of audit records available for download.
type ContentBlob struct {
	ContentType       string `json:"contentType"`
	ContentID         string `json:"contentId"`
	ContentURI        string `json:"contentUri"`
	ContentCreated    string `json:"contentCreated"`
	ContentExpiration string `json:"contentExpiration"`
}

// Expiration parses ContentExpiration, falling back to the retention window
// from now when the field is missing or malformed.
func (b ContentBlob) Expiration() time.Time {
	for _, layout := range []string{time.RFC3339Nano, apiTimeLayout + ".999999999", apiTimeLayout} {
		if t, err := time.Parse(layout, b.ContentExpiration); err == nil {
			return t.UTC()
		}
	}
	return time.Now().UTC().Add(maxRetention)
}

// apiError is the error envelope returned by the Management Activity API.
type apiError struct {
	StatusCode int
	Code       string
	Message    string
}

func (e *apiError) Error() string {
	if e.Code != "" {
		return fmt.Sprintf("o365 api: %s (%s): %s", http.StatusText(e.StatusCode), e.Code, e.Message)
	}
	return fmt.Sprintf("o365 api: status %d: %s", e.StatusCode, e.Message)
}

// ListSubscriptions returns the current subscription state for the tenant.
func (c *Client) ListSubscriptions(ctx context.Context) ([]Subscription, error) {
	u := c.activityURL("subscriptions/list", nil)
	resp, err := c.do(ctx, http.MethodGet, u)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	var subs []Subscription
	if err := json.NewDecoder(resp.Body).Decode(&subs); err != nil {
		return nil, fmt.Errorf("decode subscriptions: %w", err)
	}
	return subs, nil
}

// StartSubscription enables a content type. Enabling an already-enabled feed
// is treated as success.
func (c *Client) StartSubscription(ctx context.Context, contentType string) error {
	u := c.activityURL("subscriptions/start", url.Values{"contentType": {contentType}})
	resp, err := c.do(ctx, http.MethodPost, u)
	if err != nil {
		var apiErr *apiError
		if errors.As(err, &apiErr) && strings.Contains(strings.ToLower(apiErr.Message), "already enabled") {
			return nil
		}
		return err
	}
	resp.Body.Close()
	return nil
}

// EnsureSubscriptions starts any of the requested content types that are not
// already enabled.
func (c *Client) EnsureSubscriptions(ctx context.Context, contentTypes []string) error {
	subs, err := c.ListSubscriptions(ctx)
	if err != nil {
		return err
	}
	enabled := make(map[string]bool, len(subs))
	for _, s := range subs {
		enabled[strings.ToLower(s.ContentType)] = strings.EqualFold(s.Status, "enabled")
	}
	for _, ct := range contentTypes {
		if enabled[strings.ToLower(ct)] {
			continue
		}
		c.log.Info("starting subscription", "content_type", ct)
		if err := c.StartSubscription(ctx, ct); err != nil {
			return fmt.Errorf("start subscription %s: %w", ct, err)
		}
	}
	return nil
}

// ListContent returns every content blob published for contentType between
// start and end. The request window is split into 24h chunks and start is
// clamped to the 7-day retention window, both of which the API enforces.
func (c *Client) ListContent(ctx context.Context, contentType string, start, end time.Time) ([]ContentBlob, error) {
	start, end = start.UTC(), end.UTC()
	if earliest := time.Now().UTC().Add(-maxRetention).Add(time.Minute); start.Before(earliest) {
		c.log.Debug("clamping start time to retention window", "content_type", contentType, "requested", start, "clamped", earliest)
		start = earliest
	}
	if !start.Before(end) {
		return nil, nil
	}

	var blobs []ContentBlob
	for windowStart := start; windowStart.Before(end); {
		windowEnd := windowStart.Add(maxQueryWindow)
		if windowEnd.After(end) {
			windowEnd = end
		}
		page, err := c.listContentWindow(ctx, contentType, windowStart, windowEnd)
		if err != nil {
			return blobs, err
		}
		blobs = append(blobs, page...)
		windowStart = windowEnd
	}
	return blobs, nil
}

func (c *Client) listContentWindow(ctx context.Context, contentType string, start, end time.Time) ([]ContentBlob, error) {
	next := c.activityURL("subscriptions/content", url.Values{
		"contentType": {contentType},
		"startTime":   {start.Format(apiTimeLayout)},
		"endTime":     {end.Format(apiTimeLayout)},
	})

	var blobs []ContentBlob
	for next != "" {
		resp, err := c.do(ctx, http.MethodGet, next)
		if err != nil {
			return blobs, err
		}

		var page []ContentBlob
		err = json.NewDecoder(resp.Body).Decode(&page)
		nextPage := resp.Header.Get("NextPageUri")
		resp.Body.Close()
		if err != nil {
			return blobs, fmt.Errorf("decode content list: %w", err)
		}

		blobs = append(blobs, page...)
		next = nextPage
	}
	return blobs, nil
}

// FetchBlob downloads a content blob and streams each audit record to fn.
func (c *Client) FetchBlob(ctx context.Context, contentURI string, fn func(map[string]any) error) error {
	resp, err := c.do(ctx, http.MethodGet, contentURI)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	dec := json.NewDecoder(resp.Body)
	dec.UseNumber()
	if _, err := dec.Token(); err != nil { // opening '['
		return fmt.Errorf("decode blob: %w", err)
	}
	for dec.More() {
		var rec map[string]any
		if err := dec.Decode(&rec); err != nil {
			return fmt.Errorf("decode blob record: %w", err)
		}
		if err := fn(rec); err != nil {
			return err
		}
	}
	return nil
}

// activityURL builds a tenant-scoped activity feed URL.
func (c *Client) activityURL(path string, query url.Values) string {
	if query == nil {
		query = url.Values{}
	}
	query.Set("PublisherIdentifier", c.publisherID)
	return fmt.Sprintf("%s/api/v1.0/%s/activity/feed/%s?%s",
		c.baseURL, url.PathEscape(c.tenantID), path, query.Encode())
}

// do performs an authenticated request, retrying throttled (429) and
// transient (5xx) responses with backoff. The caller must close the body of
// a successful response.
func (c *Client) do(ctx context.Context, method, rawURL string) (*http.Response, error) {
	var lastErr error

	for attempt := 0; attempt <= c.maxRetries; attempt++ {
		if attempt > 0 {
			delay := backoff(attempt, lastErr)
			c.log.Debug("retrying request", "attempt", attempt, "delay", delay, "url", rawURL, "error", lastErr)
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-time.After(delay):
			}
		}

		token, err := c.tokens.Token(ctx)
		if err != nil {
			return nil, err
		}
		req, err := http.NewRequestWithContext(ctx, method, rawURL, nil)
		if err != nil {
			return nil, err
		}
		req.Header.Set("Authorization", "Bearer "+token)
		req.Header.Set("Accept", "application/json")

		resp, err := c.http.Do(req)
		if err != nil {
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			lastErr = err
			continue
		}
		if resp.StatusCode >= 200 && resp.StatusCode < 300 {
			return resp, nil
		}

		apiErr := readAPIError(resp)
		resp.Body.Close()
		if !retryable(resp.StatusCode) {
			return nil, apiErr
		}
		apiErr.retryAfter = parseRetryAfter(resp.Header.Get("Retry-After"))
		lastErr = apiErr
	}
	return nil, fmt.Errorf("giving up after %d retries: %w", c.maxRetries, lastErr)
}

func retryable(status int) bool {
	return status == http.StatusTooManyRequests || status == http.StatusRequestTimeout || status >= 500
}

// readAPIError decodes the API's error envelope, falling back to the raw
// body when it is not the documented shape.
func readAPIError(resp *http.Response) *retryableError {
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))

	var envelope struct {
		Error struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	out := &retryableError{apiError: apiError{StatusCode: resp.StatusCode, Message: summarise(body)}}
	if err := json.Unmarshal(body, &envelope); err == nil && envelope.Error.Code != "" {
		out.Code = envelope.Error.Code
		out.Message = envelope.Error.Message
	}
	return out
}

// retryableError carries the server's requested backoff alongside the API
// error details.
type retryableError struct {
	apiError
	retryAfter time.Duration
}

// Unwrap exposes the embedded apiError so callers can inspect the API error
// code with errors.As.
func (e *retryableError) Unwrap() error { return &e.apiError }

func parseRetryAfter(v string) time.Duration {
	if v == "" {
		return 0
	}
	if secs, err := strconv.Atoi(strings.TrimSpace(v)); err == nil && secs >= 0 {
		return time.Duration(secs) * time.Second
	}
	if t, err := http.ParseTime(v); err == nil {
		if d := time.Until(t); d > 0 {
			return d
		}
	}
	return 0
}

// backoff honours a server-supplied Retry-After, otherwise doubles from one
// second up to a 60s ceiling.
func backoff(attempt int, err error) time.Duration {
	var re *retryableError
	if errors.As(err, &re) && re.retryAfter > 0 {
		return min(re.retryAfter, 5*time.Minute)
	}
	d := time.Duration(math.Pow(2, float64(attempt-1))) * time.Second
	return min(d, 60*time.Second)
}
