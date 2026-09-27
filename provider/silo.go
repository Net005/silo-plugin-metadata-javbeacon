package provider

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// SiloClient calls Silo's own admin REST API (distinct from the JAVBeacon
// client in client.go) to trigger a metadata refresh on an already-matched
// item. It exists only because Silo's plugin SDK (as of v0.15.0) has no
// RuntimeHost RPC that lets a plugin push updated metadata or invalidate an
// item directly - POST /api/v2/admin/items/{id}/refresh-metadata is the
// closest available substitute, and it's a request any authenticated Silo
// client can make, not something reserved for in-process code the way
// Jellyfin's plugin uses ICollectionManager.
type SiloClient struct {
	baseURL    string
	apiKey     string
	httpClient *http.Client
}

// NewSiloClient builds a client for one Silo host. baseURL should be the
// internal_base_url from RuntimeHost.GetHostInfo (loopback-fast, no public
// DNS/TLS round trip needed for a same-host admin call).
func NewSiloClient(baseURL, apiKey string) *SiloClient {
	return &SiloClient{
		baseURL:    strings.TrimRight(strings.TrimSpace(baseURL), "/"),
		apiKey:     strings.TrimSpace(apiKey),
		httpClient: &http.Client{Timeout: 15 * time.Second},
	}
}

// Configured reports whether both a base URL and an API key are set.
func (c *SiloClient) Configured() bool {
	return c != nil && c.baseURL != "" && c.apiKey != ""
}

// RefreshItemMetadata calls POST /api/v2/admin/items/{id}/refresh-metadata
// with mode "complete". Silo's own OpenAPI spec documents the "quick"/
// "complete" enum with no description of what each actually re-processes;
// this used "quick" originally on the assumption that a genre/tag catch-up
// wouldn't need a full re-match, but that was never verified against a real
// instance, and a live report of collection genre tags never appearing after
// this task ran is consistent with "quick" not re-applying GetMetadata's
// genre list at all. "complete" is heavier per call, but this task only
// calls it when jellyfin_library_revision has actually moved, so the extra
// cost is bounded to real changes, not every poll.
func (c *SiloClient) RefreshItemMetadata(ctx context.Context, mediaID string) error {
	if !c.Configured() {
		return fmt.Errorf("silo: api key is not configured")
	}
	if mediaID == "" {
		return fmt.Errorf("silo: media id is required")
	}
	body, err := json.Marshal(map[string]string{"mode": "complete"})
	if err != nil {
		return fmt.Errorf("silo: encode request: %w", err)
	}
	path := fmt.Sprintf("/api/v2/admin/items/%s/refresh-metadata", mediaID)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+path, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("silo: create request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Authorization", "Bearer "+c.apiKey)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("silo: request failed: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusAccepted || (resp.StatusCode >= 200 && resp.StatusCode < 300) {
		_, _ = io.Copy(io.Discard, resp.Body)
		return nil
	}
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	return fmt.Errorf("silo: HTTP %d refreshing item %s: %s", resp.StatusCode, mediaID, strings.TrimSpace(string(raw)))
}

// UnmatchedItem is one row of GET /api/v2/libraries/unmatched-items - an item
// Silo's own scan matched no provider result for confidently enough to
// auto-apply (status "unmatched"), matched with low confidence ("ambiguous",
// e.g. below its scoring threshold even for an exact title hit missing a
// year), or has queued for a retry ("pending").
type UnmatchedItem struct {
	ContentID   string `json:"content_id"`
	ContentType string `json:"content_type"`
	LibraryID   string `json:"library_id"`
	LibraryName string `json:"library_name"`
	Status      string `json:"status"`
	Title       string `json:"title"`
	Year        int64  `json:"year"`
}

type unmatchedItemPage struct {
	Items []UnmatchedItem `json:"items"`
	Page  struct {
		HasMore    bool   `json:"has_more"`
		NextCursor string `json:"next_cursor"`
	} `json:"page"`
	Total int64 `json:"total"`
}

// ListUnmatchedItems calls GET /api/v2/libraries/unmatched-items, returning
// one page of items plus the cursor for the next one ("" when this was the
// last page).
func (c *SiloClient) ListUnmatchedItems(ctx context.Context, cursor string) ([]UnmatchedItem, string, error) {
	if !c.Configured() {
		return nil, "", fmt.Errorf("silo: api key is not configured")
	}
	path := "/api/v2/libraries/unmatched-items?limit=200"
	if cursor != "" {
		path += "&cursor=" + url.QueryEscape(cursor)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+path, nil)
	if err != nil {
		return nil, "", fmt.Errorf("silo: create request: %w", err)
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Authorization", "Bearer "+c.apiKey)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, "", fmt.Errorf("silo: request failed: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		return nil, "", fmt.Errorf("silo: HTTP %d listing unmatched items: %s", resp.StatusCode, strings.TrimSpace(string(raw)))
	}
	var page unmatchedItemPage
	if err := json.NewDecoder(resp.Body).Decode(&page); err != nil {
		return nil, "", fmt.Errorf("silo: decode unmatched items: %w", err)
	}
	next := ""
	if page.Page.HasMore {
		next = page.Page.NextCursor
	}
	return page.Items, next, nil
}

// ApplyMatch calls POST /api/v2/admin/items/{id}/match/apply, forcing Silo to
// match contentID directly to this plugin's releaseID - bypassing Silo's own
// fuzzy title/year confidence scoring entirely. This exists because that
// scoring can reject a match this plugin already knows is correct (an exact
// release-code hit via JAVBeacon's own search) purely for lacking a
// production year on the local file, which JAV releases frequently do not
// carry in their filename.
func (c *SiloClient) ApplyMatch(ctx context.Context, contentID, libraryID, releaseID string) error {
	if !c.Configured() {
		return fmt.Errorf("silo: api key is not configured")
	}
	if contentID == "" || releaseID == "" {
		return fmt.Errorf("silo: content id and release id are required")
	}
	payload := map[string]any{
		"provider_ids": map[string]string{"javbeacon": releaseID},
	}
	if libraryID != "" {
		payload["library_id"] = libraryID
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("silo: encode request: %w", err)
	}
	path := fmt.Sprintf("/api/v2/admin/items/%s/match/apply", url.PathEscape(contentID))
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+path, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("silo: create request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Authorization", "Bearer "+c.apiKey)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("silo: request failed: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		_, _ = io.Copy(io.Discard, resp.Body)
		return nil
	}
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	return fmt.Errorf("silo: HTTP %d applying match for item %s: %s", resp.StatusCode, contentID, strings.TrimSpace(string(raw)))
}

// ItemFilePaths returns every media path Silo associates with an unmatched
// item. The auto-match task uses actual filename stems, not parsed titles.
func (c *SiloClient) ItemFilePaths(ctx context.Context, contentID string) ([]string, error) {
	if !c.Configured() {
		return nil, fmt.Errorf("silo: api key is not configured")
	}
	paths := []string{}
	cursor := ""
	for pageNumber := 0; pageNumber < 100; pageNumber++ {
		path := "/api/v2/admin/items/" + url.PathEscape(contentID) + "/files?limit=200"
		if cursor != "" {
			path += "&cursor=" + url.QueryEscape(cursor)
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+path, nil)
		if err != nil {
			return nil, err
		}
		req.Header.Set("Accept", "application/json")
		req.Header.Set("Authorization", "Bearer "+c.apiKey)
		resp, err := c.httpClient.Do(req)
		if err != nil {
			return nil, err
		}
		if resp.StatusCode < 200 || resp.StatusCode >= 300 {
			raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
			resp.Body.Close()
			return nil, fmt.Errorf("silo: HTTP %d listing item files: %s", resp.StatusCode, strings.TrimSpace(string(raw)))
		}
		var result struct {
			Items []struct {
				FilePath string `json:"file_path"`
			} `json:"items"`
			Page struct {
				HasMore    bool   `json:"has_more"`
				NextCursor string `json:"next_cursor"`
			} `json:"page"`
		}
		err = json.NewDecoder(resp.Body).Decode(&result)
		resp.Body.Close()
		if err != nil {
			return nil, err
		}
		for _, item := range result.Items {
			if item.FilePath != "" {
				paths = append(paths, item.FilePath)
			}
		}
		if !result.Page.HasMore {
			return paths, nil
		}
		if result.Page.NextCursor == "" || result.Page.NextCursor == cursor {
			return nil, fmt.Errorf("silo: item file pagination did not advance")
		}
		cursor = result.Page.NextCursor
	}
	return nil, fmt.Errorf("silo: item has too many file pages")
}
