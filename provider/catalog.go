package provider

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
)

// CatalogItem is a local Silo item in a configured library.
type CatalogItem struct {
	ContentID string `json:"content_id"`
	Title     string `json:"title"`
	Type      string `json:"type"`
}

// ListLibraryCatalog uses Silo's public v2 catalog rather than a RuntimeHost
// callback from inside a scheduled task (which can deadlock that task RPC).
func (c *SiloClient) ListLibraryCatalog(ctx context.Context, libraryID string) ([]CatalogItem, error) {
	if libraryID == "" {
		return nil, fmt.Errorf("silo: library ID is required")
	}
	var profiles struct {
		Items []struct {
			ID string `json:"id"`
		} `json:"items"`
	}
	if err := c.collectionRequest(ctx, http.MethodGet, "/api/v2/profiles", nil, &profiles); err != nil {
		return nil, err
	}
	if len(profiles.Items) == 0 {
		return nil, fmt.Errorf("silo: no profile available for catalog")
	}
	profileID := profiles.Items[0].ID
	items := []CatalogItem{}
	cursor := ""
	for page := 0; page < 100; page++ {
		path := "/api/v2/catalog?library_id=" + url.QueryEscape(libraryID) + "&limit=200&skip_total=true"
		if cursor != "" {
			path += "&cursor=" + url.QueryEscape(cursor)
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+path, nil)
		if err != nil {
			return nil, err
		}
		req.Header.Set("Authorization", "Bearer "+c.apiKey)
		req.Header.Set("Accept", "application/json")
		req.Header.Set("X-Profile-Id", profileID)
		resp, err := c.httpClient.Do(req)
		if err != nil {
			return nil, err
		}
		if resp.StatusCode < 200 || resp.StatusCode >= 300 {
			resp.Body.Close()
			return nil, fmt.Errorf("silo: catalog HTTP %d", resp.StatusCode)
		}
		var data struct {
			Items []CatalogItem `json:"items"`
			Page  struct {
				HasMore    bool   `json:"has_more"`
				NextCursor string `json:"next_cursor"`
			} `json:"page"`
		}
		err = json.NewDecoder(resp.Body).Decode(&data)
		resp.Body.Close()
		if err != nil {
			return nil, err
		}
		items = append(items, data.Items...)
		if !data.Page.HasMore {
			return items, nil
		}
		if data.Page.NextCursor == "" || data.Page.NextCursor == cursor {
			return nil, fmt.Errorf("silo: catalog pagination did not advance")
		}
		cursor = data.Page.NextCursor
	}
	return nil, fmt.Errorf("silo: too many catalog pages")
}
