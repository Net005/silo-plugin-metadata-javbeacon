package main

import (
	"context"
	"fmt"
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
		return map[string]any{"status": "skipped", "reason": "no Silo API key configured (see this plugin's Collection Tag Sync setting)"}, nil
	}
	host := sdkruntime.Host()
	if host == nil {
		log.Error("match-unmatched: sdkruntime.Host() returned nil - broker not bound yet, or a prior dial failed and was never retried")
		return map[string]any{"status": "error", "error": "runtime host is not bound"}, fmt.Errorf("match-unmatched: runtime host is not bound")
	}
	// See collectionsync.go's sync() for why this particular call - a
	// RuntimeHost RPC calling back into the same host that invoked this
	// Run() - is the leading suspect for the "fails after exactly 10s"
	// reports, and why it gets timed and logged on its own rather than
	// folded into the loop below.
	hostInfoStart := time.Now()
	hostInfo, err := host.GetHostInfo(ctx)
	log.Info("match-unmatched: RuntimeHost.GetHostInfo call finished", "elapsed", time.Since(hostInfoStart), "err", err, "ctx_err", ctx.Err())
	if err != nil {
		return map[string]any{"status": "error", "error": err.Error()}, err
	}
	siloClient := provider.NewSiloClient(hostInfo.InternalBaseURL, siloKey)

	matched, skipped, failed := 0, 0, 0
	var lastErr error
	cursor := ""
	pages := 0
	for {
		pageStart := time.Now()
		items, next, err := siloClient.ListUnmatchedItems(ctx, cursor)
		pages++
		log.Info("match-unmatched: ListUnmatchedItems page finished", "page", pages, "elapsed", time.Since(pageStart), "items", len(items), "err", err)
		if err != nil {
			return map[string]any{"status": "error", "error": err.Error()}, err
		}
		for _, item := range items {
			if item.ContentType != "" && item.ContentType != "movie" {
				continue
			}
			releaseID, ok, err := s.exactReleaseIDForTitle(ctx, item.Title)
			if err != nil {
				log.Warn("match-unmatched: exactReleaseIDForTitle failed", "content_id", item.ContentID, "title", item.Title, "err", err)
				failed++
				lastErr = err
				continue
			}
			if !ok {
				skipped++
				continue
			}
			if err := siloClient.ApplyMatch(ctx, item.ContentID, item.LibraryID, releaseID); err != nil {
				log.Warn("match-unmatched: ApplyMatch failed", "content_id", item.ContentID, "release_id", releaseID, "err", err)
				failed++
				lastErr = err
				continue
			}
			matched++
		}
		if next == "" {
			break
		}
		cursor = next
	}

	summary := map[string]any{
		"status":  "ok",
		"matched": matched,
		"skipped": skipped,
		"failed":  failed,
	}
	if lastErr != nil {
		summary["status"] = "partial_failure"
		summary["last_error"] = lastErr.Error()
		return summary, lastErr
	}
	return summary, nil
}

// exactReleaseIDForTitle asks JAVBeacon's own search for title and accepts a
// hit whenever exactly one result's Code matches title exactly
// (case-insensitively, after trimming whitespace on both sides) - or, when
// several results share that exact code (a genuine JAVBeacon-side duplicate:
// confirmed live, e.g. two "THPA-15" releases from two different site/
// scraper registrations, one fully scraped and one an essentially empty
// placeholder), whenever exactly one of them strictly has the most scraped
// metadata (see selectExactReleaseID/completenessScore). A title with no
// exact code hit at all, or duplicates tied for the most metadata, both come
// back as "no confident match" rather than guessing - this task force-applies
// a match with no score threshold to fall back on, so it must never resolve
// an actual coin flip.
func (s *collectionSyncTaskServer) exactReleaseIDForTitle(ctx context.Context, title string) (string, bool, error) {
	needle := strings.TrimSpace(title)
	if !looksLikeReleaseCode(needle) {
		return "", false, nil
	}
	results, err := s.runtime.provider.Search(ctx, needle, 10)
	if err != nil {
		return "", false, err
	}
	found, ok := selectExactReleaseID(results, needle)
	if !ok {
		return "", false, nil
	}
	return strconv.FormatInt(found, 10), true, nil
}

// looksLikeReleaseCode avoids a costly catalog-wide search for ordinary
// movie titles. The task only accepts an exact release-code match anyway.
func looksLikeReleaseCode(title string) bool {
	if !strings.Contains(title, "-") || strings.ContainsAny(title, " \t\n") {
		return false
	}
	for _, r := range title {
		if r >= '0' && r <= '9' {
			return true
		}
	}
	return false
}

// selectExactReleaseID returns the release ID of the search result whose
// Code matches title exactly (case-insensitively, trimmed on both sides).
// With no exact-code hit at all, returns ok=false. With exactly one, returns
// it. With more than one (a duplicate release code), returns the single
// candidate that strictly has the most scraped metadata (completenessScore)
// - or ok=false if two or more of them tie for the best score, since there is
// then no signal to prefer one over another and a wrong forced match is
// worse than an item that stays unmatched (see exactReleaseIDForTitle's own
// doc comment). Pulled out of exactReleaseIDForTitle so it can be unit
// tested without a configured provider or network access.
func selectExactReleaseID(results []provider.Metadata, title string) (int64, bool) {
	needle := strings.TrimSpace(title)
	if needle == "" {
		return 0, false
	}
	var candidates []provider.Metadata
	for _, item := range results {
		if strings.EqualFold(strings.TrimSpace(item.Code), needle) {
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
	if tied || candidates[bestIdx].ReleaseID == 0 {
		return 0, false
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
