package provider

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
)

// CatalogItem is a local Silo item in a configured library.
type CatalogItem struct {
	ContentID   string `json:"content_id"`
	Title       string `json:"title"`
	Type        string `json:"type"`
	PosterURL   string `json:"poster_url"`
	BackdropURL string `json:"backdrop_url"`
	ReleaseDate string `json:"release_date"`
	AddedAt     string `json:"added_at"`
	UserState   struct {
		Played bool `json:"played"`
	} `json:"user_state"`
}

// ListLibraryCatalog uses Silo's public v2 catalog rather than a RuntimeHost
// callback from inside a scheduled task (which can deadlock that task RPC).
func (c *SiloClient) ListLibraryCatalog(ctx context.Context, libraryID string) ([]CatalogItem, error) {
	items, _, err := c.ListLibraryCatalogForProfile(ctx, libraryID)
	return items, err
}

func (c *SiloClient) ListLibraryCatalogForProfile(ctx context.Context, libraryID string) ([]CatalogItem, string, error) {
	if libraryID == "" {
		return nil, "", fmt.Errorf("silo: library ID is required")
	}
	var profiles struct {
		Items []struct {
			ID string `json:"id"`
		} `json:"items"`
	}
	if err := c.collectionRequest(ctx, http.MethodGet, "/api/v2/profiles", nil, &profiles); err != nil {
		return nil, "", err
	}
	if len(profiles.Items) == 0 {
		return nil, "", fmt.Errorf("silo: no profile available for catalog")
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
			return nil, "", err
		}
		req.Header.Set("Authorization", "Bearer "+c.apiKey)
		req.Header.Set("Accept", "application/json")
		req.Header.Set("X-Profile-Id", profileID)
		resp, err := c.httpClient.Do(req)
		if err != nil {
			return nil, "", err
		}
		if resp.StatusCode < 200 || resp.StatusCode >= 300 {
			resp.Body.Close()
			return nil, "", fmt.Errorf("silo: catalog HTTP %d", resp.StatusCode)
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
			return nil, "", err
		}
		items = append(items, data.Items...)
		if !data.Page.HasMore {
			return items, profileID, nil
		}
		if data.Page.NextCursor == "" || data.Page.NextCursor == cursor {
			return nil, "", fmt.Errorf("silo: catalog pagination did not advance")
		}
		cursor = data.Page.NextCursor
	}
	return nil, "", fmt.Errorf("silo: too many catalog pages")
}

// MarkWatched applies Stash/JAVBeacon watched state to the primary Silo
// profile. Silo's watch-provider importer only matches TMDB/IMDb/TVDB IDs,
// while JAV media has provider-specific IDs, so the standard import drops it.
func (c *SiloClient) MarkWatched(ctx context.Context, profileID, contentID string) error {
	if strings.TrimSpace(profileID) == "" || strings.TrimSpace(contentID) == "" {
		return fmt.Errorf("silo: profile and content IDs are required")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/api/v2/watched/"+url.PathEscape(contentID), nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+c.apiKey)
	req.Header.Set("X-Profile-Id", profileID)
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("silo: mark watched %s: HTTP %d", contentID, resp.StatusCode)
	}
	return nil
}

// MovieLibrary describes a Silo library whose local files may carry Stash
// playback history. Watch sync is not confined to the metadata plugin's
// configured JAV library.
type MovieLibrary struct {
	ID    string   `json:"id"`
	Type  string   `json:"type"`
	Paths []string `json:"paths"`
}

func (c *SiloClient) ListMovieLibraries(ctx context.Context) ([]MovieLibrary, error) {
	var data struct {
		Items []MovieLibrary `json:"items"`
	}
	if err := c.collectionRequest(ctx, http.MethodGet, "/api/v2/libraries", nil, &data); err != nil {
		return nil, err
	}
	out := make([]MovieLibrary, 0, len(data.Items))
	for _, item := range data.Items {
		if item.Type == "movies" && item.ID != "" {
			out = append(out, item)
		}
	}
	return out, nil
}
