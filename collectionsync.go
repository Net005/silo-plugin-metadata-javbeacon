package main

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/hashicorp/go-hclog"

	pluginv1 "github.com/Silo-Server/silo-plugin-sdk/pkg/pluginproto/silo/plugin/v1"
	sdkruntime "github.com/Silo-Server/silo-plugin-sdk/pkg/pluginsdk/runtime"
	"github.com/Silo-Server/silo-plugin-sdk/pkg/pluginsdk/runtimehost"
	"google.golang.org/protobuf/types/known/structpb"

	"github.com/Net005/silo-plugin-metadata-javbeacon/provider"
)

// collectionSyncTaskServer implements scheduled_task.v1 (id "collection-sync").
//
// Why this exists: Silo's plugin SDK gives a metadata_provider.v1 plugin no
// way to push updated metadata into an item Silo already matched, and no
// "this item changed, re-fetch it" notification RPC either (checked
// RuntimeHost directly - PublishEvent only reaches other plugins subscribed
// to it, not Silo's own metadata pipeline). So a saved filter set's
// membership change - which changes the "Collection: <name>" genre tags
// GetMetadata returns - sits stale in Silo until something makes Silo call
// GetMetadata again.
//
// This task closes that gap the only way available: it polls JAVBeacon's
// jellyfin_library_revision (the same change-detection signal the Jellyfin
// plugin's own background loop polls), and when it moved, calls Silo's own
// POST /api/v2/admin/items/{id}/refresh-metadata for every item this plugin
// can map to a JAVBeacon release via RuntimeHost.ListLibraryMedia. It is not
// realtime - it only runs when Silo (or its admin) triggers this scheduled
// task - but it is the closest available substitute to the push-based sync
// Jellyfin's in-process plugin gets from ICollectionManager.
type collectionSyncTaskServer struct {
	pluginv1.UnimplementedScheduledTaskServer
	runtime *runtimeServer
	// log is nil-safe (see the log() helper below) so existing tests that
	// construct this struct directly without setting it keep working.
	log                       hclog.Logger
	mu                        sync.Mutex
	running                   map[string]bool
	previousWatchlist         map[string]bool
	previousWatchlistReleases map[int64]bool
}

// log returns s.log, or a discarding no-op logger if it was never set (e.g.
// a test constructing this struct directly). Every call site here goes
// through this instead of touching s.log directly so a nil logger can never
// panic a scheduled task run.
func (s *collectionSyncTaskServer) logger() hclog.Logger {
	if s.log != nil {
		return s.log
	}
	return hclog.NewNullLogger()
}

// Run dispatches on task_key, since Silo's plugin SDK exposes only one
// ScheduledTask service per plugin process - the manifest declares each
// scheduled_task.v1 capability as a separate id, and Silo passes that id back
// as task_key on every Run call. Anything other than "match-unmatched" (an
// empty key, or the id "collection-sync") falls back to the collection-tag
// sync this struct originally implemented alone.
func (s *collectionSyncTaskServer) Run(_ context.Context, req *pluginv1.RunScheduledTaskRequest) (*pluginv1.RunScheduledTaskResponse, error) {
	taskKey := req.GetTaskKey()
	if taskKey != "match-unmatched" {
		taskKey = "collection-sync"
	}
	s.mu.Lock()
	if s.running == nil {
		s.running = make(map[string]bool)
	}
	if s.running[taskKey] {
		s.mu.Unlock()
		output, _ := structpb.NewStruct(map[string]any{"status": "already_running"})
		return &pluginv1.RunScheduledTaskResponse{Output: output}, nil
	}
	s.running[taskKey] = true
	s.mu.Unlock()
	// Silo gives this RPC a short deadline. A library-wide job must outlive it.
	go func() {
		defer func() { s.mu.Lock(); delete(s.running, taskKey); s.mu.Unlock() }()
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Hour)
		defer cancel()
		started := time.Now()
		log := s.logger()
		log.Info("scheduled task background run starting", "task_key", taskKey)
		var summary map[string]any
		var err error
		if taskKey == "match-unmatched" {
			summary, err = s.matchUnmatched(ctx)
		} else {
			summary, err = s.sync(ctx)
		}
		if err != nil {
			log.Error("scheduled task background run failed", "task_key", taskKey, "elapsed", time.Since(started), "err", err)
		} else {
			log.Info("scheduled task background run finished", "task_key", taskKey, "elapsed", time.Since(started), "summary", summary)
		}
	}()
	output, _ := structpb.NewStruct(map[string]any{"status": "started", "task_key": taskKey})
	return &pluginv1.RunScheduledTaskResponse{Output: output}, nil
}

func (s *collectionSyncTaskServer) sync(ctx context.Context) (map[string]any, error) {
	log := s.logger()
	librarySyncStart := time.Now()
	sync, err := s.runtime.provider.LibrarySync(ctx)
	log.Info("collection-sync: JAVBeacon LibrarySync call finished", "elapsed", time.Since(librarySyncStart), "err", err)
	if err != nil {
		return map[string]any{"status": "error", "error": err.Error()}, err
	}
	if sync.Revision != "" && sync.Revision == s.runtime.provider.LastSyncedRevision() {
		return map[string]any{"status": "unchanged", "revision": sync.Revision}, nil
	}

	// Refresh every release that currently needs a derived genre. Watchlist
	// membership is independent of saved filter sets, so it must be included
	// even when no filter sets exist.
	releaseIDs := collectionReleaseIDs(sync)
	stashSceneIDs := map[string]bool{}
	currentWatchlist := map[string]bool{}
	currentWatchlistReleases := map[int64]bool{}
	for _, item := range sync.Watchlist {
		if item.ReleaseID > 0 {
			currentWatchlistReleases[item.ReleaseID] = true
		}
		if item.StashSceneID != "" {
			stashSceneIDs[item.StashSceneID] = true
			currentWatchlist[item.StashSceneID] = true
		}
	}
	s.mu.Lock()
	for sceneID := range s.previousWatchlist {
		stashSceneIDs[sceneID] = true // Refresh removals too.
	}
	for releaseID := range s.previousWatchlistReleases {
		releaseIDs[releaseID] = true
	}
	s.mu.Unlock()

	host := sdkruntime.Host()
	if host == nil {
		log.Error("collection-sync: sdkruntime.Host() returned nil - broker not bound yet, or a prior dial failed and was never retried")
		return map[string]any{"status": "error", "error": "runtime host is not bound"}, fmt.Errorf("collection-sync: runtime host is not bound")
	}
	siloKey := s.runtime.provider.SiloAPIKey()
	if siloKey == "" {
		// Keep the revision pending so configuring a key later still applies
		// the current collection memberships.
		return map[string]any{"status": "skipped", "reason": "no Silo API key configured (see this plugin's Collection Tag Sync setting)"}, nil
	}
	// GetHostInfo is a RuntimeHost RPC call FROM this plugin BACK INTO the
	// Silo host, over the same broker stream the host used to invoke this
	// very Run() call - see runtime.go's pluginHostState doc comment in the
	// vendored SDK. If Silo enforces a deadline on the whole Run() RPC (the
	// leading hypothesis for the "fails after exactly 10s" reports - see
	// this task's own tracking notes), a slow or blocked call here is
	// exactly where that deadline would fire. Timing and logging it
	// separately from the RefreshItemMetadata loop below is what will
	// actually distinguish those two hypotheses next time this happens.
	hostInfoStart := time.Now()
	hostInfo, err := host.GetHostInfo(ctx)
	log.Info("collection-sync: RuntimeHost.GetHostInfo call finished", "elapsed", time.Since(hostInfoStart), "err", err, "ctx_err", ctx.Err())
	if err != nil {
		return map[string]any{"status": "error", "error": err.Error()}, err
	}
	siloClient := provider.NewSiloClient(hostInfo.InternalBaseURL, siloKey)

	listMediaStart := time.Now()
	mediaIDs, err := mapReleaseIDsToMediaIDs(ctx, host, releaseIDs, stashSceneIDs)
	log.Info("collection-sync: RuntimeHost.ListLibraryMedia paging finished", "elapsed", time.Since(listMediaStart), "matched_items", len(mediaIDs), "err", err)
	if err != nil {
		return map[string]any{"status": "error", "error": err.Error()}, err
	}

	refreshed, failed := 0, 0
	var lastErr error
	for _, mediaID := range mediaIDs {
		itemStart := time.Now()
		if err := siloClient.RefreshItemMetadata(ctx, mediaID); err != nil {
			log.Warn("collection-sync: RefreshItemMetadata failed", "media_id", mediaID, "elapsed", time.Since(itemStart), "err", err)
			failed++
			lastErr = err
			continue
		}
		refreshed++
	}
	if lastErr == nil {
		s.runtime.provider.SetLastSyncedRevision(sync.Revision)
		s.mu.Lock()
		s.previousWatchlist = currentWatchlist
		s.previousWatchlistReleases = currentWatchlistReleases
		s.mu.Unlock()
	}

	summary := map[string]any{
		"status":         "ok",
		"revision":       sync.Revision,
		"matched_items":  len(mediaIDs),
		"refreshed":      refreshed,
		"failed":         failed,
		"tracked_in_any": len(releaseIDs),
	}
	if lastErr != nil {
		summary["status"] = "partial_failure"
		summary["last_error"] = lastErr.Error()
		return summary, lastErr
	}
	return summary, nil
}

// mapReleaseIDsToMediaIDs pages through RuntimeHost.ListLibraryMedia looking
// for items this plugin itself matched (external_provider=="javbeacon"),
// returning the Silo media IDs for whichever of releaseIDs it finds. Items
// Silo hasn't matched to a JAVBeacon release at all are naturally absent from
// both the input set's hits and the output - there is nothing to refresh for
// them.
func mapReleaseIDsToMediaIDs(ctx context.Context, host *runtimehost.Client, releaseIDs map[int64]bool, stashSceneIDs map[string]bool) ([]string, error) {
	var mediaIDs []string
	pageToken := ""
	for {
		resp, err := host.ListLibraryMedia(ctx, runtimehost.ListLibraryMediaRequest{PageSize: 200, PageToken: pageToken})
		if err != nil {
			return nil, fmt.Errorf("list library media: %w", err)
		}
		for _, item := range resp.Items {
			if item.ExternalProvider != capabilityID {
				continue
			}
			id, err := strconv.ParseInt(item.ExternalID, 10, 64)
			if err == nil && releaseIDs[id] {
				mediaIDs = append(mediaIDs, item.MediaID)
				continue
			}
			if strings.HasPrefix(item.ExternalID, "stash:") && stashSceneIDs[strings.TrimPrefix(item.ExternalID, "stash:")] {
				mediaIDs = append(mediaIDs, item.MediaID)
			}
		}
		if resp.NextPageToken == "" {
			break
		}
		pageToken = resp.NextPageToken
	}
	return mediaIDs, nil
}

// collectionReleaseIDs collects both sources of Silo's derived genre tags.
func collectionReleaseIDs(snapshot *provider.LibrarySync) map[int64]bool {
	ids := map[int64]bool{}
	if snapshot == nil {
		return ids
	}
	for _, item := range snapshot.Watchlist {
		if item.ReleaseID > 0 {
			ids[item.ReleaseID] = true
		}
	}
	for _, preset := range snapshot.FilterPresets {
		for _, id := range preset.ReleaseIDs {
			if id > 0 {
				ids[id] = true
			}
		}
	}
	return ids
}
