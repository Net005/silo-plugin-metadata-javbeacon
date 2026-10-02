package main

import (
	"context"
	"fmt"
	"net/url"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	sdkruntime "github.com/Silo-Server/silo-plugin-sdk/pkg/pluginsdk/runtime"

	"github.com/Net005/silo-plugin-metadata-javbeacon/provider"
)

// matchUnmatched implements the "match-unmatched" scheduled task (see
// collectionSyncTaskServer.Run for how task_key routes here).
//
// Why this exists: Silo's own scan-time auto-matcher scores every search
// result by comparing it against the local file's title/year, and a JAV
// release code frequently has no production year available anywhere in its
// filename - the score for even an exact title match against this plugin's
// own search result then lands below Silo's auto-accept threshold, leaving
// the item sitting as "ambiguous" or "unmatched" (confirmed live: an exact
// "ADN-131" title match scored 62/100 and was not auto-applied) even though
// this plugin can tell with certainty it is the right release. Silo's plugin
// SDK gives a metadata_provider.v1 plugin no way to influence or bypass that
// scoring from inside Search/GetMetadata - but its admin REST API has a
// separate, score-free path: POST /api/v2/admin/items/{id}/match/apply forces
// a specific provider_ids match directly. This task walks Silo's own
// unmatched-items list, re-derives a release ID by treating each item's
// parsed title as a JAVBeacon release code, and force-applies the match only
// when that lookup is an exact (case-insensitive, whitespace-trimmed) code
// hit - deliberately never a fuzzy or partial one, since a wrong forced match
// is worse than an item that stays unmatched.
func (s *collectionSyncTaskServer) matchUnmatched(ctx context.Context) (map[string]any, error) {
	log := s.logger()
	siloKey := s.runtime.provider.SiloAPIKey()
	if siloKey == "" {
		return map[string]any{"status": "skipped", "reason": "no Silo API key configured (see this plugin's Silo Collection Sync setting)"}, nil
	}
	baseURL := s.runtime.provider.SiloBaseURL()
	if baseURL == "" {
		host := sdkruntime.Host()
		if host == nil {
			return nil, fmt.Errorf("match-unmatched: Silo URL is not configured and runtime host is unavailable")
		}
		hostInfo, err := host.GetHostInfo(ctx)
		if err != nil {
			return nil, err
		}
		baseURL = hostInfo.InternalBaseURL
	}
	siloClient := provider.NewSiloClient(baseURL, siloKey)

	matched, skipped, failed := 0, 0, 0
	s.mu.Lock()
	cursor, offset := s.matchCursor, s.matchOffset
	s.mu.Unlock()
	pages := 0
	for {
		if ctx.Err() != nil {
			break
		}
		items, next, err := siloClient.ListUnmatchedItems(ctx, cursor)
		if err != nil {
			return nil, err
		}
		pages++
		if offset > len(items) {
			offset = 0
		}
		for i := offset; i < len(items); i++ {
			if ctx.Err() != nil || shortDeadline(ctx) || matched >= 400 {
				return s.matchPartial(cursor, i, matched, skipped, failed, pages, "batch_limit")
			}
			item := items[i]
			if item.ContentType != "" && item.ContentType != "movie" {
				skipped++
				continue
			}
			providerID, ok, err := s.exactProviderIDForItem(ctx, siloClient, item)
			if err != nil {
				if strings.Contains(err.Error(), "HTTP 429") {
					return s.matchPartial(cursor, i, matched, skipped, failed, pages, "rate_limited")
				}
				failed++
				log.Warn("auto-match lookup failed", "content_id", item.ContentID, "err", err)
				continue
			}
			if !ok {
				skipped++
				continue
			}
			if err := siloClient.ApplyMatch(ctx, item.ContentID, item.LibraryID, providerID); err != nil {
				if strings.Contains(err.Error(), "HTTP 429") {
					return s.matchPartial(cursor, i, matched, skipped, failed, pages, "rate_limited")
				}
				failed++
				log.Warn("auto-match apply failed", "content_id", item.ContentID, "err", err)
				continue
			}
			matched++
		}
		offset = 0
		if next == "" {
			cursor = ""
			break
		}
		cursor = next
	}
	s.mu.Lock()
	s.matchCursor = cursor
	s.matchOffset = offset
	s.mu.Unlock()
	return map[string]any{"status": "ok", "matched": matched, "skipped": skipped, "failed": failed, "pages": pages, "remaining": false}, nil
}

// exactProviderIDForItem checks every media filename before the parsed title.
// Distinct matches across multiple files are ambiguous and never forced.
func (s *collectionSyncTaskServer) exactProviderIDForItem(ctx context.Context, client *provider.SiloClient, item provider.UnmatchedItem) (string, bool, error) {
	paths, err := client.ItemFilePaths(ctx, item.ContentID)
	if err != nil {
		return "", false, err
	}
	seen := map[string]bool{}
	found := ""
	for _, path := range paths {
		base := filepath.Base(path)
		stem := strings.TrimSuffix(base, filepath.Ext(base))
		stem = strings.TrimSpace(stem)
		if stem == "" || seen[strings.ToLower(stem)] {
			continue
		}
		seen[strings.ToLower(stem)] = true
		candidates := []string{stem}
		if code := filenameReleaseCode(stem); code != "" && normalizedCatalogCode(code) != normalizedCatalogCode(stem) {
			candidates = append([]string{code}, candidates...)
		}
		id, ok := "", false
		for _, candidate := range candidates {
			var err error
			id, ok, err = s.exactProviderIDForTitle(ctx, candidate)
			if err != nil {
				return "", false, err
			}
			if ok {
				break
			}
		}
		if !ok {
			continue
		}
		if found != "" && found != id {
			return "", false, nil
		}
		found = id
	}
	if found != "" {
		return found, true, nil
	}
	// Older Silo versions may not report media files for an unmatched item.
	if len(paths) == 0 {
		return s.exactProviderIDForTitle(ctx, item.Title)
	}
	return "", false, nil
}

var filenameReleaseCodePattern = regexp.MustCompile(`(?i)^\s*[\[(]?\s*([a-z]{2,12})[-_ ]*([0-9]{1,6})(?:\b|[_ .-])`)

// filenameReleaseCode extracts a leading code from a file carrying quality
// suffixes. The full filename remains the Stash-only fallback candidate.
func filenameReleaseCode(stem string) string {
	match := filenameReleaseCodePattern.FindStringSubmatch(stem)
	if len(match) != 3 {
		return ""
	}
	return match[1] + "-" + match[2]
}

// exactProviderIDForTitle accepts only separator-equivalent exact codes.
// Duplicate releases favor the most complete metadata, with a stable ID
// tie-break when equally complete.
func (s *collectionSyncTaskServer) exactProviderIDForTitle(ctx context.Context, title string) (string, bool, error) {
	needle := strings.TrimSpace(title)
	if needle == "" {
		return "", false, nil
	}
	results, err := s.runtime.provider.Search(ctx, needle, 10)
	if err != nil {
		return "", false, err
	}
	found, ok := selectExactReleaseID(results, needle)
	if ok {
		return strconv.FormatInt(found, 10), true, nil
	}
	// A Stash-only scene has a stable provider ID but no JAVBeacon release ID.
	// Accept it only when exactly one scene's code/file stem matches.
	providerID, ok := selectExactStashProviderID(results, needle)
	return providerID, ok, nil
}

func selectExactStashProviderID(results []provider.Metadata, needle string) (string, bool) {
	providerID := ""
	for _, result := range results {
		if normalizedCatalogCode(result.Code) != normalizedCatalogCode(needle) || !strings.HasPrefix(result.ProviderID, "stash:") {
			continue
		}
		if providerID != "" {
			return "", false
		}
		providerID = result.ProviderID
	}
	return providerID, providerID != ""
}

// selectExactReleaseID returns the release ID of the search result whose
// Code matches title exactly (case-insensitively, trimmed on both sides).
// With no exact-code hit at all, returns ok=false. With exactly one, returns
// it. With more than one (a duplicate release code), returns the single
// candidate that strictly has the most scraped metadata (completenessScore)
// - with a stable release-ID tie-break when scores are equal. Pulled out so
// it can be tested without a configured provider or network access.
func selectExactReleaseID(results []provider.Metadata, title string) (int64, bool) {
	needle := strings.TrimSpace(title)
	if needle == "" {
		return 0, false
	}
	var candidates []provider.Metadata
	for _, item := range results {
		if normalizedCatalogCode(item.Code) == normalizedCatalogCode(needle) {
			candidates = append(candidates, item)
		}
	}
	if len(candidates) == 0 {
		return 0, false
	}
	bestIdx := 0
	bestScore := completenessScore(candidates[0])
	tied := false
	for i := 1; i < len(candidates); i++ {
		score := completenessScore(candidates[i])
		switch {
		case score > bestScore:
			bestIdx, bestScore, tied = i, score, false
		case score == bestScore:
			tied = true
		}
	}
	if candidates[bestIdx].ReleaseID == 0 {
		return 0, false
	}
	// Two records with the same exact code and equal metadata are equivalent
	// for matching. Pick a stable winner so repeat scans do not disagree.
	if tied {
		for i, candidate := range candidates {
			if completenessScore(candidate) == bestScore && candidate.ReleaseID > 0 && candidate.ReleaseID < candidates[bestIdx].ReleaseID {
				bestIdx = i
			}
		}
	}
	return candidates[bestIdx].ReleaseID, true
}

// completenessScore is a rough "how much did JAVBeacon actually scrape for
// this release" signal, used only to break a tie between two or more search
// results sharing the same exact release code. Weighted toward fields a real
// scrape either clearly has or clearly doesn't (a release date, a cast list,
// a linked StashApp scene) over less telling ones - the exact weights matter
// far less than the ordering, which is unlikely to be close in practice: a
// duplicate is normally one fully-scraped row and one essentially-empty
// placeholder, not two competitively-scraped rows.
func completenessScore(m provider.Metadata) int {
	score := 0
	if strings.TrimSpace(m.PremiereDate) != "" {
		score += 2
	}
	if len(m.Performers) > 0 {
		score += 2
	}
	if strings.TrimSpace(m.StashSceneID) != "" {
		score += 2
	}
	if strings.TrimSpace(m.Studio) != "" {
		score++
	}
	if len(m.Genres) > 0 {
		score++
	}
	if len(m.Directors) > 0 {
		score++
	}
	if m.RuntimeSeconds > 0 {
		score++
	}
	if strings.TrimSpace(m.Overview) != "" {
		score++
	}
	return score
}

func shortDeadline(ctx context.Context) bool {
	deadline, ok := ctx.Deadline()
	return ok && time.Until(deadline) < 1500*time.Millisecond
}

func (s *collectionSyncTaskServer) matchPartial(cursor string, offset, matched, skipped, failed, pages int, reason string) (map[string]any, error) {
	s.mu.Lock()
	s.matchCursor = cursor
	s.matchOffset = offset
	s.mu.Unlock()
	return map[string]any{"status": "partial", "reason": reason, "matched": matched, "skipped": skipped, "failed": failed, "pages": pages, "remaining": true}, nil
}

// stashSceneFromArtwork reads only the scene ID from an existing JAVBeacon
// artwork URL. It never trusts a title alone to overwrite an existing match.
var stashSceneArtwork = regexp.MustCompile(`^/api/v1/integrations/silo/stash/scenes/([0-9]+)/`)

func stashSceneFromArtwork(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return ""
	}
	match := stashSceneArtwork.FindStringSubmatch(u.Path)
	if len(match) != 2 {
		return ""
	}
	return match[1]
}

// repairMatchedWithoutCast catches Silo's scan-time partial matches. Those
// items do not appear in /libraries/unmatched-items, although applying the
// exact JAV release+scene pair immediately hydrates performers and metadata.
func (s *collectionSyncTaskServer) repairMatchedWithoutCast(ctx context.Context, client *provider.SiloClient) (int, error) {
	libraries, err := client.ListJAVBeaconMovieLibraries(ctx)
	if err != nil {
		return 0, err
	}
	profileID, err := client.PrimaryProfileID(ctx)
	if err != nil {
		return 0, err
	}
	repaired := 0
	for _, library := range libraries {
		if ctx.Err() != nil || shortDeadline(ctx) {
			break
		}
		s.mu.Lock()
		backlog := s.repairCursor[library.ID]
		s.mu.Unlock()
		// Always examine the newest page. The second page advances a cursor
		// through older matches, so recent additions need not wait for a
		// whole-library sweep to finish.
		pages := []string{""}
		if backlog != "" {
			pages = append(pages, backlog)
		}
		for _, cursor := range pages {
			if ctx.Err() != nil || shortDeadline(ctx) {
				break
			}
			items, next, err := client.ListMatchedCatalogPage(ctx, library.ID, cursor)
			if err != nil {
				return repaired, err
			}
			completed := true
			for _, item := range items {
				if ctx.Err() != nil || shortDeadline(ctx) {
					completed = false
					break
				}
				if item.Status != "matched" || item.Type != "movie" {
					continue
				}
				sceneID := stashSceneFromArtwork(item.PosterURL)
				if sceneID == "" {
					sceneID = stashSceneFromArtwork(item.BackdropURL)
				}
				if sceneID == "" {
					continue
				}
				s.mu.Lock()
				lastChecked := s.repairChecked[item.ContentID]
				s.mu.Unlock()
				if time.Since(lastChecked) < 15*time.Minute {
					continue
				}
				if strings.TrimSpace(item.Overview) != "" || len(item.Genres) != 0 {
					hasCast, err := client.ItemHasCast(ctx, profileID, item.ContentID)
					if err != nil {
						return repaired, err
					}
					if hasCast {
						s.markRepairChecked(item.ContentID)
						continue
					}
				}
				results, err := s.runtime.provider.Search(ctx, item.Title, 10)
				if err != nil {
					return repaired, err
				}
				providerID := exactProviderForScene(results, item.Title, sceneID)
				if providerID == "" {
					s.markRepairChecked(item.ContentID)
					continue
				}
				if err := client.ApplyMatchWithStash(ctx, item.ContentID, library.ID, providerID, sceneID); err != nil {
					return repaired, err
				}
				s.markRepairChecked(item.ContentID)
				repaired++
			}
			if !completed {
				break
			}
			s.mu.Lock()
			if s.repairCursor == nil {
				s.repairCursor = map[string]string{}
			}
			s.repairCursor[library.ID] = next
			s.mu.Unlock()
		}
	}
	return repaired, nil
}

func (s *collectionSyncTaskServer) markRepairChecked(contentID string) {
	s.mu.Lock()
	if s.repairChecked == nil {
		s.repairChecked = map[string]time.Time{}
	}
	s.repairChecked[contentID] = time.Now()
	s.mu.Unlock()
}

// exactProviderForScene requires both an exact code and the same Stash scene
// already represented by Silo. A duplicate code tied to another scene cannot
// overwrite the existing local item.
func exactProviderForScene(results []provider.Metadata, title, sceneID string) string {
	bestID, bestScore := int64(0), -1
	stashAvailable := false
	for _, result := range results {
		if result.StashSceneID != sceneID || normalizedCatalogCode(result.Code) != normalizedCatalogCode(title) || len(result.Performers) == 0 {
			continue
		}
		if result.ReleaseID > 0 {
			score := completenessScore(result)
			if bestID == 0 || score > bestScore || (score == bestScore && result.ReleaseID < bestID) {
				bestID, bestScore = result.ReleaseID, score
			}
		} else if result.ProviderID == "stash:"+sceneID {
			stashAvailable = true
		}
	}
	if bestID > 0 {
		return strconv.FormatInt(bestID, 10)
	}
	if stashAvailable {
		return "stash:" + sceneID
	}
	return ""
}
