package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"path/filepath"
	"strings"
	"time"

	"github.com/Net005/silo-plugin-metadata-javbeacon/provider"
)

// pollMetadata is deliberately separate from collection reconciliation. A
// slow collection run must never postpone a new release or Stash scene edit.
func (s *collectionSyncTaskServer) pollMetadata() {
	var since time.Time
	ticker := time.NewTicker(15 * time.Second)
	defer ticker.Stop()
	for {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
		next, count, err := s.syncChangedMetadata(ctx, since)
		cancel()
		if err != nil {
			s.logger().Warn("incremental metadata refresh failed; retaining cursor for retry", "since", since, "error", err)
		} else if !next.IsZero() {
			since = next
			if count > 0 {
				s.logger().Info("incremental metadata refresh queued", "items", count)
			}
		}
		<-ticker.C
	}
}

// Silo's local content ID is the first 14 bytes of SHA-256 of the exact
// indexed file path. This was checked against multiple live item/file pairs.
func siloLocalContentID(path string) string {
	sum := sha256.Sum256([]byte(path))
	return "local-" + hex.EncodeToString(sum[:14])
}

func metadataChangeCandidates(change provider.MetadataChange) []string {
	candidates := []string{change.Code, change.Title}
	if change.Path != "" {
		base := filepath.Base(change.Path)
		candidates = append(candidates, strings.TrimSuffix(base, filepath.Ext(base)))
	}
	return candidates
}

// resolveChangedItems requires a unique exact catalog title per identity.
// Refreshing an unrelated local file is worse than leaving one edit pending.
type resolvedMetadataItem struct {
	ID   string
	Path string
}

func resolveChangedItems(changes []provider.MetadataChange, catalog []provider.CatalogItem) []resolvedMetadataItem {
	byTitle := map[string][]string{}
	for _, item := range catalog {
		if item.Type != "movie" || item.ContentID == "" {
			continue
		}
		key := normalizedCatalogCode(item.Title)
		byTitle[key] = append(byTitle[key], item.ContentID)
	}
	items := []resolvedMetadataItem{}
	seen := map[string]bool{}
	for _, change := range changes {
		for _, candidate := range metadataChangeCandidates(change) {
			if strings.TrimSpace(candidate) == "" {
				continue
			}
			matches := byTitle[normalizedCatalogCode(candidate)]
			if len(matches) != 1 {
				continue
			}
			if !seen[matches[0]] {
				items = append(items, resolvedMetadataItem{ID: matches[0], Path: change.Path})
				seen[matches[0]] = true
			}
			break
		}
	}
	return items
}

func (s *collectionSyncTaskServer) syncChangedMetadata(ctx context.Context, since time.Time) (time.Time, int, error) {
	p := s.runtime.provider
	if p.SiloAPIKey() == "" || p.SiloBaseURL() == "" || p.SiloLibraryID() == "" {
		return time.Time{}, 0, nil
	}
	feed, err := p.MetadataChanges(ctx, since)
	if err != nil {
		return time.Time{}, 0, err
	}
	if feed.CheckedAt.IsZero() || feed.CheckedAt.Before(since) {
		return time.Time{}, 0, fmt.Errorf("invalid metadata change cursor")
	}
	if len(feed.Items) == 0 {
		if err := p.AckMetadataChanges(ctx, feed.CheckedAt); err != nil {
			return time.Time{}, 0, err
		}
		return feed.CheckedAt, 0, nil
	}
	client := provider.NewSiloClient(p.SiloBaseURL(), p.SiloAPIKey())
	ids := []string{}
	seen := map[string]bool{}
	fallback := []provider.MetadataChange{}
	checkedPaths := map[string]bool{}
	for _, change := range feed.Items {
		if change.Path == "" {
			fallback = append(fallback, change)
			continue
		}
		id := siloLocalContentID(change.Path)
		if checkedPaths[id] {
			continue
		}
		checkedPaths[id] = true
		exists, err := client.ItemHasFileInLibrary(ctx, id, p.SiloLibraryID(), change.Path)
		if err != nil {
			return time.Time{}, 0, err
		}
		if !exists {
			fallback = append(fallback, change)
			continue
		}
		if !seen[id] {
			ids = append(ids, id)
			seen[id] = true
		}
	}
	if len(fallback) > 0 {
		catalog, _, err := client.ListLibraryCatalogForProfile(ctx, p.SiloLibraryID())
		if err != nil {
			return time.Time{}, 0, err
		}
		for _, item := range resolveChangedItems(fallback, catalog) {
			if seen[item.ID] {
				continue
			}
			if item.Path != "" {
				exists, err := client.ItemHasFileInLibrary(ctx, item.ID, p.SiloLibraryID(), item.Path)
				if err != nil {
					return time.Time{}, 0, err
				}
				if !exists {
					continue
				}
			}
			ids = append(ids, item.ID)
			seen[item.ID] = true
		}
	}
	p.ClearMetadataCache()
	for i, id := range ids {
		if err := client.RefreshItemMetadata(ctx, id); err != nil {
			return time.Time{}, i, err
		}
		// Keep Silo's job queue responsive without flooding it after a burst of
		// Stash hooks. Ordinary single-item changes are sent immediately.
		if i+1 < len(ids) {
			select {
			case <-time.After(200 * time.Millisecond):
			case <-ctx.Done():
				return time.Time{}, i + 1, ctx.Err()
			}
		}
	}
	if err := p.AckMetadataChanges(ctx, feed.CheckedAt); err != nil {
		return time.Time{}, len(ids), err
	}
	return feed.CheckedAt, len(ids), nil
}
