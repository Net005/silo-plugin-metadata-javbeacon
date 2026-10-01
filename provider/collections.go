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
	"time"
)

// CollectionSpec is one saved filter set's matched Silo media in one library.
type CollectionSpec struct {
	Kind      string
	PresetID  int64
	Name      string
	LibraryID string
	MediaIDs  []string
	Artwork   []CollectionArtwork
}

// CollectionArtwork is a local collection member's existing Silo artwork.
type CollectionArtwork struct {
	MediaID     string
	PosterURL   string
	BackdropURL string
	ReleaseDate string
	AddedAt     string
}

const collectionOwner = "Managed by JAVBeacon metadata plugin."

type siloCollection struct {
	ID           string                     `json:"id"`
	Title        string                     `json:"title"`
	GroupID      *string                    `json:"group_id"`
	LibraryID    string                     `json:"library_id"`
	Slug         string                     `json:"slug"`
	Description  string                     `json:"description"`
	PosterURL    string                     `json:"poster_url"`
	BackdropURL  string                     `json:"backdrop_url"`
	SourceConfig map[string]json.RawMessage `json:"source_config"`
}

func collectionSlug(spec CollectionSpec) string {
	if spec.Kind == "watchlist" {
		return "javbeacon-watchlist-library-" + strings.ToLower(spec.LibraryID)
	}
	return "javbeacon-preset-" + strconv.FormatInt(spec.PresetID, 10) + "-library-" + strings.ToLower(spec.LibraryID)
}

func (c *SiloClient) collectionRequest(ctx context.Context, method, path string, payload any, target any) error {
	_, err := c.collectionRequestETag(ctx, method, path, payload, target, "")
	return err
}

func (c *SiloClient) collectionRequestETag(ctx context.Context, method, path string, payload any, target any, etag string) (string, error) {
	if !c.Configured() {
		return "", fmt.Errorf("silo: api key is not configured")
	}
	var body io.Reader
	if payload != nil {
		raw, err := json.Marshal(payload)
		if err != nil {
			return "", err
		}
		body = bytes.NewReader(raw)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.baseURL+path, body)
	if err != nil {
		return "", err
	}
	req.Header.Set("Authorization", "Bearer "+c.apiKey)
	if etag != "" {
		req.Header.Set("If-Match", etag)
	}
	req.Header.Set("Accept", "application/json")
	if payload != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return "", fmt.Errorf("silo: collection %s %s: HTTP %d: %s", method, path, resp.StatusCode, strings.TrimSpace(string(raw)))
	}
	if target != nil {
		return resp.Header.Get("ETag"), json.NewDecoder(io.LimitReader(resp.Body, 4<<20)).Decode(target)
	}
	return resp.Header.Get("ETag"), nil
}

func (c *SiloClient) collections(ctx context.Context) ([]siloCollection, error) {
	var response struct {
		Items []siloCollection `json:"items"`
	}
	if err := c.collectionRequest(ctx, http.MethodGet, "/api/v2/admin/collections", nil, &response); err != nil {
		return nil, err
	}
	return response.Items, nil
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
	changed, _, err := c.SyncCollectionsBatch(ctx, specs, 0)
	return changed, err
}

// SyncCollectionsBatch limits writes per invocation so Silo scheduled tasks
// can resume safely instead of exceeding the task RPC deadline or API quota.
func (c *SiloClient) SyncCollectionsBatch(ctx context.Context, specs []CollectionSpec, maxChanges int, pruneUnselected ...bool) (int, bool, error) {
	existing, err := c.collections(ctx)
	if err != nil {
		return 0, false, err
	}
	bySlug := map[string]siloCollection{}
	for _, item := range existing {
		bySlug[item.Slug] = item
	}
	desired := map[string]CollectionSpec{}
	activeLibraries := map[string]bool{}
	for _, spec := range specs {
		activeLibraries[spec.LibraryID] = true
		if (spec.Kind != "watchlist" && (spec.Kind != "preset" || spec.PresetID <= 0)) || spec.LibraryID == "" {
			continue
		}
		desired[collectionSlug(spec)] = spec
	}
	prune := len(pruneUnselected) > 0 && pruneUnselected[0]
	// Reconcile formerly managed collections too when a saved preset is
	// removed or no matching media remain. Keep the empty collection rather
	// than deleting an administrator-visible object without a restore path.
	for _, item := range existing {
		if (strings.HasPrefix(item.Slug, "javbeacon-preset-") || strings.HasPrefix(item.Slug, "javbeacon-watchlist-")) && strings.HasPrefix(item.Description, collectionOwner) {
			if _, ok := desired[item.Slug]; !ok && !(prune && strings.HasPrefix(item.Slug, "javbeacon-preset-")) {
				desired[item.Slug] = CollectionSpec{LibraryID: item.LibraryID}
			}
		}
	}
	// Silo displays ungrouped collections in manual order. Keep our own
	// collection slots alphabetized while preserving every other collection's
	// relative position and group membership.
	orderChanges, err := c.sortManagedCollections(ctx, existing)
	if err != nil {
		return 0, false, err
	}
	changed := orderChanges
	if orderChanges > 0 {
		if maxChanges > 0 && changed >= maxChanges {
			return changed, false, nil
		}
	}
	slugs := make([]string, 0, len(desired))
	for slug := range desired {
		slugs = append(slugs, slug)
	}
	sort.Strings(slugs)
	// A nonblank selection removes only plugin-owned preset collections that
	// are no longer selected. User-owned and Watchlist collections are kept.
	if prune {
		for _, item := range existing {
			if !strings.HasPrefix(item.Slug, "javbeacon-preset-") || !strings.HasPrefix(item.Description, collectionOwner) || !activeLibraries[item.LibraryID] {
				continue
			}
			if _, ok := desired[item.Slug]; ok {
				continue
			}
			if collectionDeadlineNear(ctx) {
				return changed, false, nil
			}
			if err := c.collectionRequest(ctx, http.MethodDelete, "/api/v2/admin/collections/"+url.PathEscape(item.ID), nil, nil); err != nil {
				return changed, false, err
			}
			changed++
			if maxChanges > 0 && changed >= maxChanges {
				return changed, false, nil
			}
		}
	}
	createdAny := false
	for _, slug := range slugs {
		if collectionDeadlineNear(ctx) {
			return changed, false, nil
		}
		spec := desired[slug]
		collection, exists := bySlug[slug]
		if exists && (!strings.HasPrefix(collection.Description, collectionOwner) || collection.LibraryID != spec.LibraryID) {
			return changed, false, fmt.Errorf("silo: collection slug %q belongs to another owner", slug)
		}
		if !exists {
			if spec.Name == "" {
				continue
			}
			payload := map[string]any{"title": spec.Name, "slug": slug, "collection_type": "manual", "library_id": spec.LibraryID, "description": collectionOwner + " " + spec.Kind + " ID: " + strconv.FormatInt(spec.PresetID, 10)}
			if err := c.collectionRequest(ctx, http.MethodPost, "/api/v2/admin/collections", payload, &collection); err != nil {
				return changed, false, err
			}
			changed++
			createdAny = true
			if maxChanges > 0 && changed >= maxChanges {
				return changed, false, nil
			}
		}
		if exists && spec.Name != "" && collection.Title != spec.Name {
			path := "/api/v2/admin/collections/" + url.PathEscape(collection.ID)
			var current json.RawMessage
			etag, err := c.collectionRequestETag(ctx, http.MethodGet, path, nil, &current, "")
			if err != nil {
				return changed, false, err
			}
			if etag == "" {
				return changed, false, fmt.Errorf("silo: collection title ETag is missing")
			}
			if _, err := c.collectionRequestETag(ctx, http.MethodPatch, path, map[string]string{"title": spec.Name}, nil, etag); err != nil {
				return changed, false, err
			}
			changed++
			if maxChanges > 0 && changed >= maxChanges {
				return changed, false, nil
			}
		}
		members, err := c.collectionMembers(ctx, collection.ID)
		if err != nil {
			return changed, false, err
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
			if collectionDeadlineNear(ctx) {
				return changed, false, nil
			}
			if _, exists := members[mediaID]; exists {
				continue
			}
			path := "/api/v2/admin/collections/" + url.PathEscape(collection.ID) + "/items/" + url.PathEscape(mediaID)
			if err := c.collectionRequest(ctx, http.MethodPut, path, map[string]int{"position": position}, nil); err != nil {
				return changed, false, err
			}
			changed++
			if maxChanges > 0 && changed >= maxChanges {
				return changed, false, nil
			}
		}
		for mediaID := range members {
			if collectionDeadlineNear(ctx) {
				return changed, false, nil
			}
			if want[mediaID] {
				continue
			}
			path := "/api/v2/admin/collections/" + url.PathEscape(collection.ID) + "/items/" + url.PathEscape(mediaID)
			if err := c.collectionRequest(ctx, http.MethodDelete, path, nil, nil); err != nil {
				return changed, false, err
			}
			changed++
			if maxChanges > 0 && changed >= maxChanges {
				return changed, false, nil
			}
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
			if collectionDeadlineNear(ctx) {
				return changed, false, nil
			}
			path := "/api/v2/admin/collections/" + url.PathEscape(collection.ID) + "/items/order"
			var current json.RawMessage
			etag, err := c.collectionRequestETag(ctx, http.MethodGet, path, nil, &current, "")
			if err != nil {
				return changed, false, err
			}
			if etag == "" {
				return changed, false, fmt.Errorf("silo: collection order ETag is missing")
			}
			if _, err := c.collectionRequestETag(ctx, http.MethodPut, path, map[string]any{"ordered_ids": ordered}, nil, etag); err != nil {
				return changed, false, err
			}
		}
		if collectionDeadlineNear(ctx) {
			return changed, false, nil
		}
		artChanged, err := c.syncCollectionArtwork(ctx, collection, spec, time.Now())
		if err != nil {
			return changed, false, err
		}
		if artChanged {
			changed++
			if maxChanges > 0 && changed >= maxChanges {
				return changed, false, nil
			}
		}
	}
	if createdAny {
		updated, err := c.collections(ctx)
		if err != nil {
			return changed, false, err
		}
		orderChanges, err := c.sortManagedCollections(ctx, updated)
		changed += orderChanges
		if err != nil {
			return changed, false, err
		}
	}
	return changed, true, nil
}

// alphabetizeManagedSlots sorts only plugin-owned collections into their
// existing slots. Other collections keep their positions in Silo's list.
func alphabetizeManagedSlots(ids []string, byID map[string]siloCollection) ([]string, bool) {
	ordered := append([]string(nil), ids...)
	slots := []int{}
	managed := []string{}
	for index, id := range ids {
		item, ok := byID[id]
		if ok && item.GroupID == nil && strings.HasPrefix(item.Description, collectionOwner) && (strings.HasPrefix(item.Slug, "javbeacon-preset-") || strings.HasPrefix(item.Slug, "javbeacon-watchlist-")) {
			slots = append(slots, index)
			managed = append(managed, id)
		}
	}
	sort.SliceStable(managed, func(i, j int) bool {
		a := strings.ToLower(byID[managed[i]].Title)
		b := strings.ToLower(byID[managed[j]].Title)
		if a == b {
			return managed[i] < managed[j]
		}
		return a < b
	})
	changed := false
	for i, slot := range slots {
		if ordered[slot] != managed[i] {
			changed = true
		}
		ordered[slot] = managed[i]
	}
	return ordered, changed
}

func (c *SiloClient) sortManagedCollections(ctx context.Context, existing []siloCollection) (int, error) {
	byID := make(map[string]siloCollection, len(existing))
	libraries := map[string]bool{}
	for _, item := range existing {
		byID[item.ID] = item
		if item.GroupID == nil && strings.HasPrefix(item.Description, collectionOwner) && (strings.HasPrefix(item.Slug, "javbeacon-preset-") || strings.HasPrefix(item.Slug, "javbeacon-watchlist-")) {
			libraries[item.LibraryID] = true
		}
	}
	libraryIDs := make([]string, 0, len(libraries))
	for libraryID := range libraries {
		libraryIDs = append(libraryIDs, libraryID)
	}
	sort.Strings(libraryIDs)
	changes := 0
	for _, libraryID := range libraryIDs {
		path := "/api/v2/admin/collections/order?library_id=" + url.QueryEscape(libraryID)
		var current struct {
			OrderedIDs []string `json:"ordered_ids"`
		}
		etag, err := c.collectionRequestETag(ctx, http.MethodGet, path, nil, &current, "")
		if err != nil {
			return changes, err
		}
		ordered, changed := alphabetizeManagedSlots(current.OrderedIDs, byID)
		if !changed {
			continue
		}
		if etag == "" {
			return changes, fmt.Errorf("silo: collection list order ETag is missing")
		}
		if _, err := c.collectionRequestETag(ctx, http.MethodPut, "/api/v2/admin/collections/order", map[string]any{"library_id": libraryID, "ordered_ids": ordered}, nil, etag); err != nil {
			return changes, err
		}
		changes++
	}
	return changes, nil
}

// Keep enough time to return partial progress before Silo's task RPC deadline.
func collectionDeadlineNear(ctx context.Context) bool {
	deadline, ok := ctx.Deadline()
	return ok && time.Until(deadline) < 1500*time.Millisecond
}
