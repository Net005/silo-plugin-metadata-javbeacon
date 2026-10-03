package main

import (
	"context"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"time"

	"github.com/Net005/silo-plugin-metadata-javbeacon/provider"
)

var releaseArtworkPath = regexp.MustCompile(`^/covers/([0-9]+)(?:/|$)`)

func releaseFromArtwork(raw string) int64 {
	u, err := url.Parse(raw)
	if err != nil {
		return 0
	}
	match := releaseArtworkPath.FindStringSubmatch(u.Path)
	if len(match) != 2 {
		return 0
	}
	id, _ := strconv.ParseInt(match[1], 10, 64)
	return id
}

func (s *collectionSyncTaskServer) startPlayBackfill() (<-chan collectionSyncResult, bool) {
	s.mu.Lock()
	if s.running == nil {
		s.running = make(map[string]bool)
	}
	if s.running["play-backfill"] {
		s.mu.Unlock()
		return nil, false
	}
	s.running["play-backfill"] = true
	s.mu.Unlock()
	done := make(chan collectionSyncResult, 1)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
		defer cancel()
		summary, err := s.backfillCompletedPlays(ctx)
		if err != nil {
			s.logger().Error("Silo play backfill failed", "error", err)
		} else {
			s.logger().Info("Silo play backfill completed", "summary", summary)
		}
		s.mu.Lock()
		delete(s.running, "play-backfill")
		s.mu.Unlock()
		done <- collectionSyncResult{summary, err}
	}()
	return done, true
}

func (s *collectionSyncTaskServer) backfillCompletedPlays(ctx context.Context) (map[string]any, error) {
	p := s.runtime.provider
	if !p.Configured() || p.SiloAPIKey() == "" || p.SiloBaseURL() == "" {
		return map[string]any{"status": "skipped", "reason": "JAVBeacon and Silo connections are required"}, nil
	}
	silo := provider.NewSiloClient(p.SiloBaseURL(), p.SiloAPIKey())
	profileID, err := silo.PrimaryProfileID(ctx)
	if err != nil {
		return nil, err
	}
	attempts, err := silo.CompletedPlaybackHistory(ctx, profileID)
	if err != nil {
		return nil, err
	}
	grouped := map[string][]provider.SiloBackfillPlay{}
	skipped := 0
	for _, attempt := range attempts {
		if !attempt.Completed || attempt.MediaType != "movie" || attempt.SessionID == "" || attempt.MediaItemID == "" || attempt.EndedAt.IsZero() {
			skipped++
			continue
		}
		poster, backdrop, err := silo.ItemArtwork(ctx, profileID, attempt.MediaItemID)
		if err != nil {
			skipped++
			continue
		}
		sceneID := stashSceneFromArtwork(poster)
		if sceneID == "" {
			sceneID = stashSceneFromArtwork(backdrop)
		}
		if sceneID == "" {
			releaseID := releaseFromArtwork(poster)
			if releaseID == 0 {
				releaseID = releaseFromArtwork(backdrop)
			}
			if releaseID > 0 {
				metadata, err := p.GetMetadata(ctx, releaseID)
				if err != nil {
					return nil, err
				}
				if metadata != nil {
					sceneID = metadata.StashSceneID
				}
			}
		}
		if sceneID == "" {
			skipped++
			continue
		}
		grouped[sceneID] = append(grouped[sceneID], provider.SiloBackfillPlay{SessionID: attempt.SessionID, EndedAt: attempt.EndedAt})
	}
	scenes := make([]string, 0, len(grouped))
	for id := range grouped {
		scenes = append(scenes, id)
	}
	sort.Strings(scenes)
	summary := map[string]any{"status": "ok", "completed_attempts": len(attempts), "matched_scenes": len(scenes), "added": 0, "already_present": 0, "skipped": skipped}
	for _, sceneID := range scenes {
		plays := grouped[sceneID]
		if len(plays) > 200 {
			summary["skipped"] = summary["skipped"].(int) + len(plays)
			continue
		}
		result, err := p.BackfillSiloPlays(ctx, sceneID, plays)
		if err != nil {
			return summary, err
		}
		summary["added"] = summary["added"].(int) + result.Added
		summary["already_present"] = summary["already_present"].(int) + result.AlreadyPresent
		summary["skipped"] = summary["skipped"].(int) + result.Skipped
		if result.Reason != "" {
			s.logger().Info("Silo play backfill skipped scene", "scene_id", sceneID, "reason", result.Reason)
		}
	}
	return summary, nil
}
