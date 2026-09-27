package main

import (
	"context"
	"fmt"
	"strconv"
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
	log hclog.Logger
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
func (s *collectionSyncTaskServer) Run(ctx context.Context, req *pluginv1.RunScheduledTaskRequest) (*pluginv1.RunScheduledTaskResponse, error) {
	taskKey := req.GetTaskKey()
	log := s.logger()
	started := time.Now()
	log.Info("scheduled task run starting", "task_key", taskKey)
	// Silo's own admin UI only ever shows "Task failed. Inspect
	// administrator diagnostics for details." on error, with no further
	// text (confirmed live) - this Info/Error pair around the whole call is
	// what actually makes that diagnostics text findable, in Silo's own Logs
	// page, for whatever this plugin's own log() calls below didn't already
	// narrow down. Recorded even on success so a run's actual duration is
	// visible without cross-referencing the admin UI's own timer.
	var summary map[string]any
	var err error
	if taskKey == "match-unmatched" {
		summary, err = s.matchUnmatched(ctx)
	} else {
		summary, err = s.sync(ctx)
	}
	elapsed := time.Since(started)
	if err != nil {
		log.Error("scheduled task run failed", "task_key", taskKey, "elapsed", elapsed, "err", err, "ctx_err", ctx.Err())
	} else {
		log.Info("scheduled task run finished", "task_key", taskKey, "elapsed", elapsed, "summary", summary)
	}
	output, encodeErr := structpb.NewStruct(summary)
	if encodeErr != nil {
		// Encoding our own summary failing is not worth turning a successful
		// sync into a reported failure - fall back to an empty output struct.
		output = &structpb.Struct{}
	}
	return &pluginv1.RunScheduledTaskResponse{Output: output}, err
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

	// releaseIDs is every release ID that appears in ANY saved filter set.
	// Refreshing the union (rather than diffing exactly which presets a
	// release joined/left since last time) is deliberately coarser but much
	// simpler and self-correcting - a release that already has its correct
	// genre tags just gets a harmless no-op "complete" refresh alongside the
	// ones that actually changed.
	releaseIDs := map[int64]bool{}
	for _, preset := range sync.FilterPresets {
		for _, id := range preset.ReleaseIDs {
			releaseIDs[id] = true
		}
	}

	host := sdkruntime.Host()
	if host == nil {
		log.Error("collection-sync: sdkruntime.Host() returned nil - broker not bound yet, or a prior dial failed and was never retried")
		return map[string]any{"status": "error", "error": "runtime host is not bound"}, fmt.Errorf("collection-sync: runtime host is not bound")
	}
	siloKey := s.runtime.provider.SiloAPIKey()
	if siloKey == "" {
		// Not an error: the admin simply hasn't opted into this feature by
		// setting a Silo API key yet. Still record the revision so a later
		// key configuration starts fresh rather than immediately firing a
		// backlog of refreshes for changes that accumulated while it was off.
		s.runtime.provider.SetLastSyncedRevision(sync.Revision)
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
	mediaIDs, err := mapReleaseIDsToMediaIDs(ctx, host, releaseIDs)
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
	s.runtime.provider.SetLastSyncedRevision(sync.Revision)

	summary := map[string]any{
		"status":         "ok",
		"revision":       sync.Revision,
		"matched_items":  len(mediaIDs),
		"refreshed":      refreshed,
		"failed":         failed,
		"tracked_in_any": len(releaseIDs),
	}
	if lastErr != nil {
		summary["last_error"] = lastErr.Error()
	}
	return summary, nil
}

// mapReleaseIDsToMediaIDs pages through RuntimeHost.ListLibraryMedia looking
// for items this plugin itself matched (external_provider=="javbeacon"),
// returning the Silo media IDs for whichever of releaseIDs it finds. Items
// Silo hasn't matched to a JAVBeacon release at all are naturally absent from
// both the input set's hits and the output - there is nothing to refresh for
// them.
func mapReleaseIDsToMediaIDs(ctx context.Context, host *runtimehost.Client, releaseIDs map[int64]bool) ([]string, error) {
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
			if err != nil || !releaseIDs[id] {
				continue
			}
			mediaIDs = append(mediaIDs, item.MediaID)
		}
		if resp.NextPageToken == "" {
			break
		}
		pageToken = resp.NextPageToken
	}
	return mediaIDs, nil
}
