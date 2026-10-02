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
	ticker := time.NewTicker(15 * time.Second)
	defer ticker.Stop()
	for {
		s.startMetadataSync()
		<-ticker.C
	}
}

func (s *collectionSyncTaskServer) startMetadataSync() (<-chan collectionSyncResult, bool) {
	s.mu.Lock()
	if s.running == nil {
		s.running = make(map[string]bool)
	}
	if s.running["metadata-refresh"] {
		s.mu.Unlock()
		return nil, false
	}
	s.running["metadata-refresh"] = true
	since := s.metadataSince
	s.mu.Unlock()
	done := make(chan collectionSyncResult, 1)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
		defer cancel()
		next, count, err := s.syncChangedMetadata(ctx, since)
		if err != nil {
			s.logger().Warn("incremental metadata refresh failed; retaining cursor for retry", "since", since, "error", err)
		}
		summary := map[string]any{"status": "ok", "submitted": count}
		s.mu.Lock()
		if err == nil && !next.IsZero() {
			s.metadataSince = next
		}
		delete(s.running, "metadata-refresh")
		s.mu.Unlock()
		if err != nil {
			summary["status"] = "error"
		}
		done <- collectionSyncResult{summary, err}
	}()
	return done, true
}

type metadataRefreshBatch struct {
	checkedAt time.Time
	items     []metadataRefreshItem
}

type metadataRefreshItem struct {
	contentID string
	jobID     string
	attempts  int
	retryAt   time.Time
	done      bool
}

// advanceMetadataBatch submits at most 50 new jobs per poll and only
// acknowledges a source cursor after every Silo job actually succeeds.
func (s *collectionSyncTaskServer) advanceMetadataBatch(ctx context.Context, client *provider.SiloClient) (time.Time, int, error) {
	batch := s.metadataPending
	if batch == nil {
		return time.Time{}, 0, nil
	}
	now := time.Now()
	for i := range batch.items {
		item := &batch.items[i]
		if item.done || item.jobID == "" {
			continue
		}
		state, err := client.MetadataJobState(ctx, item.jobID)
		if err != nil {
			return time.Time{}, 0, err
		}
		switch state {
		case "succeeded":
			item.done = true
		case "failed", "canceled":
			s.logger().Warn("Silo metadata refresh job failed; will retry", "content_id", item.contentID, "job_id", item.jobID, "attempts", item.attempts)
			item.jobID = ""
			delay := time.Minute
			if item.attempts >= 3 {
				delay = 10 * time.Minute
			}
			item.retryAt = now.Add(delay)
		case "queued", "running":
		default:
			return time.Time{}, 0, fmt.Errorf("unexpected Silo metadata job state %q", state)
		}
	}
	submitted := 0
	for i := range batch.items {
		item := &batch.items[i]
		if item.done || item.jobID != "" || now.Before(item.retryAt) {
			continue
		}
		if submitted >= 50 {
			break
		}
		jobID, err := client.RefreshItemMetadata(ctx, item.contentID)
		if err != nil {
			return time.Time{}, submitted, err
		}
		item.jobID = jobID
		item.attempts++
		submitted++
		if submitted < 50 {
			select {
			case <-time.After(100 * time.Millisecond):
			case <-ctx.Done():
				return time.Time{}, submitted, ctx.Err()
			}
		}
	}
	for _, item := range batch.items {
		if !item.done {
			return time.Time{}, submitted, nil
		}
	}
	if err := s.runtime.provider.AckMetadataChanges(ctx, batch.checkedAt); err != nil {
		return time.Time{}, submitted, err
	}
	checked := batch.checkedAt
	s.metadataPending = nil
	return checked, submitted, nil
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

func libraryForMetadataPath(path string, libraries []provider.MovieLibrary) string {
	bestID, bestLength := "", 0
	for _, library := range libraries {
		for _, root := range library.Paths {
			root = strings.TrimRight(root, "/")
			if root != "" && (path == root || strings.HasPrefix(path, root+"/")) && len(root) > bestLength {
				bestID, bestLength = library.ID, len(root)
			}
		}
	}
	return bestID
}

func (s *collectionSyncTaskServer) syncChangedMetadata(ctx context.Context, since time.Time) (time.Time, int, error) {
	p := s.runtime.provider
	if p.SiloAPIKey() == "" || p.SiloBaseURL() == "" || p.SiloLibraryID() == "" {
		return time.Time{}, 0, nil
	}
	client := provider.NewSiloClient(p.SiloBaseURL(), p.SiloAPIKey())
	if s.metadataPending != nil {
		return s.advanceMetadataBatch(ctx, client)
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
	libraries, err := client.ListMovieLibraries(ctx)
	if err != nil {
		return time.Time{}, 0, err
	}
	ids := []string{}
	seen := map[string]bool{}
	fallback := map[string][]provider.MetadataChange{}
	checkedPaths := map[string]bool{}
	for _, change := range feed.Items {
		libraryID := p.SiloLibraryID()
		if change.Path != "" {
			libraryID = libraryForMetadataPath(change.Path, libraries)
			if libraryID == "" {
				continue
			}
		}
		if change.Path == "" {
			fallback[libraryID] = append(fallback[libraryID], change)
			continue
		}
		id := siloLocalContentID(change.Path)
		if checkedPaths[id] {
			continue
		}
		checkedPaths[id] = true
		exists, err := client.ItemHasFileInLibrary(ctx, id, libraryID, change.Path)
		if err != nil {
			return time.Time{}, 0, err
		}
		if !exists {
			fallback[libraryID] = append(fallback[libraryID], change)
			continue
		}
		if !seen[id] {
			ids = append(ids, id)
			seen[id] = true
		}
	}
	for libraryID, changes := range fallback {
		catalog, _, err := client.ListLibraryCatalogForProfile(ctx, libraryID)
		if err != nil {
			return time.Time{}, 0, err
		}
		for _, item := range resolveChangedItems(changes, catalog) {
			if seen[item.ID] {
				continue
			}
			if item.Path != "" {
				exists, err := client.ItemHasFileInLibrary(ctx, item.ID, libraryID, item.Path)
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
	if len(ids) == 0 {
		if err := p.AckMetadataChanges(ctx, feed.CheckedAt); err != nil {
			return time.Time{}, 0, err
		}
		return feed.CheckedAt, 0, nil
	}
	batch := &metadataRefreshBatch{checkedAt: feed.CheckedAt, items: make([]metadataRefreshItem, 0, len(ids))}
	for _, id := range ids {
		batch.items = append(batch.items, metadataRefreshItem{contentID: id})
	}
	s.metadataPending = batch
	return s.advanceMetadataBatch(ctx, client)
}
