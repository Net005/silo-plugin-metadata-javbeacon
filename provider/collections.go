package provider

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
)

// CollectionSpec is one saved filter set's matched Silo media in one library.
type CollectionSpec struct {
	Kind      string
	PresetID  int64
	Name      string
	LibraryID string
	MediaIDs  []string
}

const collectionOwner = "Managed by JAVBeacon metadata plugin."

type siloCollection struct {
	ID          string `json:"id"`
	LibraryID   string `json:"library_id"`
	Slug        string `json:"slug"`
	Description string `json:"description"`
}

func collectionSlug(spec CollectionSpec) string {
	if spec.Kind == "watchlist" {
		return "javbeacon-watchlist-library-" + strings.ToLower(spec.LibraryID)
	}
	return "javbeacon-preset-" + strconv.FormatInt(spec.PresetID, 10) + "-library-" + strings.ToLower(spec.LibraryID)
}

func (c *SiloClient) collectionRequest(ctx context.Context, method, path string, payload any, target any) error {
	if !c.Configured() {
		return fmt.Errorf("silo: api key is not configured")
	}
	var body io.Reader
	if payload != nil {
		raw, err := json.Marshal(payload)
		if err != nil {
			return err
		}
		body = bytes.NewReader(raw)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.baseURL+path, body)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+c.apiKey)
	req.Header.Set("Accept", "application/json")
	if payload != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return fmt.Errorf("silo: collection %s %s: HTTP %d: %s", method, path, resp.StatusCode, strings.TrimSpace(string(raw)))
	}
	if target != nil {
		return json.NewDecoder(io.LimitReader(resp.Body, 4<<20)).Decode(target)
	}
	return nil
}

func (c *SiloClient) collections(ctx context.Context) ([]siloCollection, error) {
	out := []siloCollection{}
	cursor := ""
	for page := 0; page < 100; page++ {
		path := "/api/v2/admin/collections?limit=200"
		if cursor != "" {
			path += "&cursor=" + url.QueryEscape(cursor)
		}
		var response struct {
			Items []siloCollection `json:"items"`
			Page  struct {
				HasMore    bool   `json:"has_more"`
				NextCursor string `json:"next_cursor"`
			} `json:"page"`
		}
		if err := c.collectionRequest(ctx, http.MethodGet, path, nil, &response); err != nil {
			return nil, err
		}
		out = append(out, response.Items...)
		if !response.Page.HasMore {
			return out, nil
		}
		if response.Page.NextCursor == "" || response.Page.NextCursor == cursor {
			return nil, fmt.Errorf("silo: collection pagination did not advance")
		}
		cursor = response.Page.NextCursor
	}
	return nil, fmt.Errorf("silo: too many collection pages")
}

func (c *SiloClient) collectionMembers(ctx context.Context, id string) (map[string]int, error) {
	out := map[string]int{}
	cursor := ""
	for page := 0; page < 100; page++ {
		path := "/api/v2/admin/collections/" + url.PathEscape(id) + "/items?limit=200"
		if cursor != "" {
			path += "&cursor=" + url.QueryEscape(cursor)
		}
		var response struct {
			Items []struct {
				MediaItemID string `json:"media_item_id"`
				Position    int    `json:"position"`
			} `json:"items"`
			Page struct {
				HasMore    bool   `json:"has_more"`
				NextCursor string `json:"next_cursor"`
			} `json:"page"`
		}
		if err := c.collectionRequest(ctx, http.MethodGet, path, nil, &response); err != nil {
			return nil, err
		}
		for _, item := range response.Items {
			out[item.MediaItemID] = item.Position
		}
		if !response.Page.HasMore {
			return out, nil
		}
		if response.Page.NextCursor == "" || response.Page.NextCursor == cursor {
			return nil, fmt.Errorf("silo: collection member pagination did not advance")
		}
		cursor = response.Page.NextCursor
	}
	return nil, fmt.Errorf("silo: too many collection member pages")
}

// SyncCollections owns only collections carrying its stable slug and
// marker. Existing user collections, even with the same title, are untouched.
func (c *SiloClient) SyncCollections(ctx context.Context, specs []CollectionSpec) (int, error) {
	existing, err := c.collections(ctx)
	if err != nil {
		return 0, err
	}
	bySlug := map[string]siloCollection{}
	for _, item := range existing {
		bySlug[item.Slug] = item
	}
	desired := map[string]CollectionSpec{}
	for _, spec := range specs {
		if (spec.Kind != "watchlist" && (spec.Kind != "preset" || spec.PresetID <= 0)) || spec.LibraryID == "" {
			continue
		}
		desired[collectionSlug(spec)] = spec
	}
	// Reconcile formerly managed collections too when a saved preset is
	// removed or no matching media remain. Keep the empty collection rather
	// than deleting an administrator-visible object without a restore path.
	for _, item := range existing {
		if (strings.HasPrefix(item.Slug, "javbeacon-preset-") || strings.HasPrefix(item.Slug, "javbeacon-watchlist-")) && strings.HasPrefix(item.Description, collectionOwner) {
			if _, ok := desired[item.Slug]; !ok {
				desired[item.Slug] = CollectionSpec{LibraryID: item.LibraryID}
			}
		}
	}
	slugs := make([]string, 0, len(desired))
	for slug := range desired {
		slugs = append(slugs, slug)
	}
	sort.Strings(slugs)
	changed := 0
	for _, slug := range slugs {
		spec := desired[slug]
		collection, exists := bySlug[slug]
		if exists && (!strings.HasPrefix(collection.Description, collectionOwner) || collection.LibraryID != spec.LibraryID) {
			return changed, fmt.Errorf("silo: collection slug %q belongs to another owner", slug)
		}
		if !exists {
			if spec.Name == "" {
				continue
			}
			payload := map[string]any{"title": spec.Name, "slug": slug, "collection_type": "manual", "library_id": spec.LibraryID, "description": collectionOwner + " " + spec.Kind + " ID: " + strconv.FormatInt(spec.PresetID, 10)}
			if err := c.collectionRequest(ctx, http.MethodPost, "/api/v2/admin/collections", payload, &collection); err != nil {
				return changed, err
			}
			changed++
		}
		members, err := c.collectionMembers(ctx, collection.ID)
		if err != nil {
			return changed, err
		}
		want := map[string]bool{}
		ordered := []string{}
		for _, mediaID := range spec.MediaIDs {
			if mediaID != "" && !want[mediaID] {
				ordered = append(ordered, mediaID)
				want[mediaID] = true
			}
		}
		for position, mediaID := range ordered {
			if _, exists := members[mediaID]; exists {
				continue
			}
			path := "/api/v2/admin/collections/" + url.PathEscape(collection.ID) + "/items/" + url.PathEscape(mediaID)
			if err := c.collectionRequest(ctx, http.MethodPut, path, map[string]int{"position": position}, nil); err != nil {
				return changed, err
			}
			changed++
		}
		for mediaID := range members {
			if want[mediaID] {
				continue
			}
			path := "/api/v2/admin/collections/" + url.PathEscape(collection.ID) + "/items/" + url.PathEscape(mediaID)
			if err := c.collectionRequest(ctx, http.MethodDelete, path, nil, nil); err != nil {
				return changed, err
			}
			changed++
		}
		needsOrder := len(ordered) > 0
		if len(members) == len(ordered) {
			needsOrder = false
			for index, id := range ordered {
				if members[id] != index {
					needsOrder = true
					break
				}
			}
		}
		if needsOrder {
			path := "/api/v2/admin/collections/" + url.PathEscape(collection.ID) + "/items/order"
			if err := c.collectionRequest(ctx, http.MethodPut, path, map[string]any{"ordered_ids": ordered}, nil); err != nil {
				return changed, err
			}
		}

	}
	return changed, nil
}
