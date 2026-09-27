package provider

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	defaultHTTPTimeout = 30 * time.Second
	maxResponseBody    = 4 << 20 // 4 MiB - generous for a releases-search page of JSON.
)

var defaultHTTPClient = &http.Client{Timeout: defaultHTTPTimeout}

// StatusError is any non-2xx response from JAVBeacon.
type StatusError struct {
	StatusCode int
	Body       string
}

func (e *StatusError) Error() string {
	if e.Body == "" {
		return fmt.Sprintf("javbeacon: HTTP %d", e.StatusCode)
	}
	return fmt.Sprintf("javbeacon: HTTP %d: %s", e.StatusCode, e.Body)
}

// Client talks to one JAVBeacon instance's Silo integration API
// (/api/v1/integrations/silo/...). It is intentionally not built on
// pluginsdk/httpclient: that package sends a fixed X-Api-Key header, while
// JAVBeacon only accepts "Authorization: Bearer <key>" or an "?api_key="
// query parameter (see internal/web/server.go's security() middleware in the
// JAVBeacon repo) - so this plugin carries its own small, equally bounded
// client instead.
type Client struct {
	baseURL       string
	apiKey        string
	httpClient    *http.Client
	cacheMu       sync.Mutex
	metadataCache map[int64]cachedMetadata
	stashCache    map[string]cachedMetadata
	inflight      map[int64]chan struct{}
}

// cachedMetadata prevents Silo's GetMetadata and GetImages calls for the same
// release from repeating the expensive StashApp enrichment in JAVBeacon.
type cachedMetadata struct {
	item    *Metadata
	expires time.Time
}

// NewClient builds a client for one JAVBeacon instance. A nil hc gets a
// default client. baseURL is right-trimmed of "/"; apiKey is space-trimmed.
func NewClient(baseURL, apiKey string, hc *http.Client) *Client {
	if hc == nil {
		hc = defaultHTTPClient
	}
	return &Client{
		baseURL:    strings.TrimRight(strings.TrimSpace(baseURL), "/"),
		apiKey:     strings.TrimSpace(apiKey),
		httpClient: hc,
	}
}

// Configured reports whether both a base URL and an API key are set.
func (c *Client) Configured() bool {
	return c != nil && c.baseURL != "" && c.apiKey != ""
}

func (c *Client) getJSON(ctx context.Context, path string, dest any) error {
	if c.baseURL == "" {
		return fmt.Errorf("javbeacon: base url is required")
	}
	if c.apiKey == "" {
		return fmt.Errorf("javbeacon: api key is required")
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+path, nil)
	if err != nil {
		return fmt.Errorf("javbeacon: create request: %w", err)
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Authorization", "Bearer "+c.apiKey)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("javbeacon: request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, maxResponseBody))
		return &StatusError{StatusCode: resp.StatusCode, Body: strings.TrimSpace(string(raw))}
	}
	if dest == nil || resp.StatusCode == http.StatusNoContent {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, maxResponseBody))
		return nil
	}
	err = json.NewDecoder(io.LimitReader(resp.Body, maxResponseBody)).Decode(dest)
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, maxResponseBody))
	if err != nil && err != io.EOF {
		return fmt.Errorf("javbeacon: decode response: %w", err)
	}
	return nil
}

// Search calls GET /api/v1/integrations/silo/search.
func (c *Client) Search(ctx context.Context, query string, limit int) ([]Metadata, error) {
	values := url.Values{}
	if query != "" {
		values.Set("q", query)
	}
	if limit > 0 {
		values.Set("limit", strconv.Itoa(limit))
	}
	path := "/api/v1/integrations/silo/search"
	if encoded := values.Encode(); encoded != "" {
		path += "?" + encoded
	}
	var out searchResponse
	if err := c.getJSON(ctx, path, &out); err != nil {
		return nil, err
	}
	return out.Items, nil
}

// GetMetadata calls GET /api/v1/integrations/silo/releases/{id}. It returns
// (nil, nil) when JAVBeacon has no release with that id, matching the "not
// found is not an error" convention the metadata_provider.v1 contract
// expects (an empty GetMetadataResponse rather than a fault).
func (c *Client) GetMetadata(ctx context.Context, releaseID int64) (*Metadata, error) {
	for {
		c.cacheMu.Lock()
		if entry, ok := c.metadataCache[releaseID]; ok && time.Now().Before(entry.expires) {
			c.cacheMu.Unlock()
			return entry.item, nil
		}
		if done := c.inflight[releaseID]; done != nil {
			c.cacheMu.Unlock()
			select {
			case <-done:
				continue
			case <-ctx.Done():
				return nil, ctx.Err()
			}
		}
		if c.inflight == nil {
			c.inflight = make(map[int64]chan struct{})
		}
		done := make(chan struct{})
		c.inflight[releaseID] = done
		c.cacheMu.Unlock()

		var out Metadata
		err := c.getJSON(ctx, fmt.Sprintf("/api/v1/integrations/silo/releases/%d", releaseID), &out)
		var item *Metadata
		if err == nil {
			item = &out
		} else {
			var statusErr *StatusError
			if isNotFound(err, &statusErr) {
				err = nil
			}
		}
		c.cacheMu.Lock()
		if err == nil {
			if c.metadataCache == nil {
				c.metadataCache = make(map[int64]cachedMetadata)
			}
			if len(c.metadataCache) >= 2048 {
				c.metadataCache = make(map[int64]cachedMetadata)
			}
			c.metadataCache[releaseID] = cachedMetadata{item: item, expires: time.Now().Add(5 * time.Minute)}
		}
		delete(c.inflight, releaseID)
		close(done)
		c.cacheMu.Unlock()
		return item, err
	}
}

// GetStashMetadata fetches a Stash-only scene through JAVBeacon's integration
// endpoint. It uses the same short-lived cache as release metadata so Silo's
// GetMetadata and GetImages calls do not repeat the GraphQL lookup.
func (c *Client) GetStashMetadata(ctx context.Context, sceneID string) (*Metadata, error) {
	c.cacheMu.Lock()
	if entry, ok := c.stashCache[sceneID]; ok && time.Now().Before(entry.expires) {
		c.cacheMu.Unlock()
		return entry.item, nil
	}
	c.cacheMu.Unlock()
	var out Metadata
	err := c.getJSON(ctx, "/api/v1/integrations/silo/stash/scenes/"+url.PathEscape(sceneID), &out)
	if err != nil {
		return nil, err
	}
	c.cacheMu.Lock()
	if c.stashCache == nil {
		c.stashCache = map[string]cachedMetadata{}
	}
	if len(c.stashCache) >= 2048 {
		c.stashCache = map[string]cachedMetadata{}
	}
	c.stashCache[sceneID] = cachedMetadata{item: &out, expires: time.Now().Add(5 * time.Minute)}
	c.cacheMu.Unlock()
	return &out, nil
}

// ClearMetadataCache makes a collection revision refresh fetch fresh genres.
func (c *Client) ClearMetadataCache() {
	c.cacheMu.Lock()
	c.metadataCache = nil
	c.stashCache = nil
	c.cacheMu.Unlock()
}

// ReportPlayback calls POST /api/v1/integrations/silo/playback, forwarding a
// playback/scrobble event into JAVBeacon's own playback engine (the same one
// backing the Jellyfin plugin), which owns Stash checkpointing, resume,
// completion thresholds, and play-count/O-count writeback.
func (c *Client) ReportPlayback(ctx context.Context, event PlaybackEvent) (*PlaybackResult, error) {
	if c.baseURL == "" {
		return nil, fmt.Errorf("javbeacon: base url is required")
	}
	if c.apiKey == "" {
		return nil, fmt.Errorf("javbeacon: api key is required")
	}
	body, err := json.Marshal(event)
	if err != nil {
		return nil, fmt.Errorf("javbeacon: encode playback event: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/api/v1/integrations/silo/playback", bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("javbeacon: create request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Authorization", "Bearer "+c.apiKey)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("javbeacon: request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, maxResponseBody))
		return nil, &StatusError{StatusCode: resp.StatusCode, Body: strings.TrimSpace(string(raw))}
	}
	var out PlaybackResult
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxResponseBody)).Decode(&out); err != nil && err != io.EOF {
		return nil, fmt.Errorf("javbeacon: decode response: %w", err)
	}
	return &out, nil
}

// LibrarySync calls GET /api/v1/integrations/silo/library-sync, returning the
// current revision and saved-filter-set membership snapshot.
func (c *Client) LibrarySync(ctx context.Context) (*LibrarySync, error) {
	var out LibrarySync
	if err := c.getJSON(ctx, "/api/v1/integrations/silo/library-sync", &out); err != nil {
		return nil, err
	}
	return &out, nil
}

func isNotFound(err error, target **StatusError) bool {
	se, ok := err.(*StatusError)
	if !ok {
		return false
	}
	*target = se
	return se.StatusCode == http.StatusNotFound
}

// PublicURL resolves a route that needs no JAVBeacon API key, such as the
// performer-to-Stash redirect used as a Silo person homepage.
func (c *Client) PublicURL(rawPath string) string {
	if c == nil || c.baseURL == "" || !strings.HasPrefix(rawPath, "/") {
		return ""
	}
	return c.baseURL + rawPath
}

// ImageURL resolves a JAVBeacon-relative path (as carried inside a
// "javbeacon://" canonical path - see javbeaconCanonicalPath in main.go) into
// a full, authenticated URL. The variant size hint is intentionally ignored:
// JAVBeacon does not generate multiple resolutions of a cover or screenshot,
// so every variant resolves to the same URL, which the
// image_resolver.v1/ResolveImageURL contract explicitly allows ("a plugin
// receiving an unknown variant MUST degrade gracefully ... and MUST NOT
// return an error").
func (c *Client) ImageURL(rawPath string) string {
	if rawPath == "" || c.baseURL == "" {
		return ""
	}
	if !strings.HasPrefix(rawPath, "/") {
		rawPath = "/" + rawPath
	}
	u, err := url.Parse(c.baseURL + rawPath)
	if err != nil {
		return ""
	}
	query := u.Query()
	query.Set("api_key", c.apiKey)
	u.RawQuery = query.Encode()
	return u.String()
}
