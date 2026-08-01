// Package o365 is a minimal client for the Office 365 Management Activity
// API: subscription management, content listing and blob retrieval.
package o365

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/alex-j-butler/o365-log-extractor/internal/msapi"
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
	// MaxQueryWindow is the largest time range the content endpoint accepts
	// in a single request.
	MaxQueryWindow = 24 * time.Hour
	// MaxRetention is how far back content blobs remain available.
	MaxRetention = 7 * 24 * time.Hour
	// apiTimeLayout is the timestamp format the API expects and returns.
	apiTimeLayout = "2006-01-02T15:04:05"
)

// Client talks to the Management Activity API for a single tenant.
type Client struct {
	api         *msapi.Client
	baseURL     string
	tenantID    string
	publisherID string
	log         *slog.Logger
}

// Options configures a Client.
type Options struct {
	Cloud        msapi.Cloud
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
	logger := opts.Logger
	if logger == nil {
		logger = slog.Default()
	}
	publisher := opts.PublisherID
	if publisher == "" {
		publisher = opts.TenantID
	}

	tokens := msapi.NewTokenSource(
		opts.Cloud.LoginURL, opts.TenantID, opts.ClientID, opts.ClientSecret,
		opts.Cloud.ManagementURL, opts.HTTPClient,
	)
	return &Client{
		api:         msapi.NewClient(tokens, opts.HTTPClient, logger, opts.MaxRetries),
		baseURL:     strings.TrimSuffix(opts.Cloud.ManagementURL, "/"),
		tenantID:    opts.TenantID,
		publisherID: publisher,
		log:         logger,
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
	return time.Now().UTC().Add(MaxRetention)
}

// ListSubscriptions returns the current subscription state for the tenant.
func (c *Client) ListSubscriptions(ctx context.Context) ([]Subscription, error) {
	resp, err := c.api.Do(ctx, http.MethodGet, c.activityURL("subscriptions/list", nil))
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
	resp, err := c.api.Do(ctx, http.MethodPost, u)
	if err != nil {
		var apiErr *msapi.APIError
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
	if earliest := time.Now().UTC().Add(-MaxRetention).Add(time.Minute); start.Before(earliest) {
		c.log.Warn("start time predates the 7-day retention window; reading from the earliest available content",
			"content_type", contentType, "requested", start, "clamped", earliest)
		start = earliest
	}
	if !start.Before(end) {
		return nil, nil
	}

	var blobs []ContentBlob
	for windowStart := start; windowStart.Before(end); {
		windowEnd := windowStart.Add(MaxQueryWindow)
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
		resp, err := c.api.Do(ctx, http.MethodGet, next)
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
	resp, err := c.api.Do(ctx, http.MethodGet, contentURI)
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
