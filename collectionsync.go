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

// collectionSyncTaskServer maintains ordered Silo collections for the StashApp
// Watchlist and JAVBeacon saved filter sets. The same sync runs on a schedule
// and from a short revision polling loop.
type collectionSyncTaskServer struct {
	pluginv1.UnimplementedScheduledTaskServer
	runtime *runtimeServer
	// log is nil-safe (see the log() helper below) so existing tests that
	// construct this struct directly without setting it keep working.
	log     hclog.Logger
	mu      sync.Mutex
	running map[string]bool
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

func (s *collectionSyncTaskServer) poll() {
	ticker := time.NewTicker(time.Minute)
	defer ticker.Stop()
	// Configure can precede the RuntimeHost broker binding; the first tick retries.
	for range ticker.C {
		s.mu.Lock()
		if s.running == nil {
			s.running = make(map[string]bool)
		}
		busy := s.running["collection-sync"]
		if !busy {
			s.running["collection-sync"] = true
		}
		s.mu.Unlock()
		if busy {
			continue
		}
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
		_, err := s.sync(ctx)
		cancel()
		if err != nil {
			s.logger().Warn("collection poll failed", "err", err)
		}
		s.mu.Lock()
		delete(s.running, "collection-sync")
		s.mu.Unlock()
	}
}

func (s *collectionSyncTaskServer) sync(ctx context.Context) (map[string]any, error) {
	snapshot, err := s.runtime.provider.LibrarySync(ctx)
	if err != nil {
		return nil, err
	}
	host := sdkruntime.Host()
	if host == nil {
		return nil, fmt.Errorf("collection-sync: runtime host is not bound")
	}
	key := s.runtime.provider.SiloAPIKey()
	if key == "" {
		return map[string]any{"status": "skipped", "reason": "Silo API key is not configured"}, nil
	}
	hostInfo, err := host.GetHostInfo(ctx)
	if err != nil {
		return nil, err
	}
	media, err := listJAVMedia(ctx, host)
	if err != nil {
		return nil, err
	}
	specs := collectionSpecs(snapshot, media)
	changed, err := provider.NewSiloClient(hostInfo.InternalBaseURL, key).SyncCollections(ctx, specs)
	if err != nil {
		return map[string]any{"status": "error", "error": err.Error()}, err
	}
	s.runtime.provider.SetLastSyncedRevision(snapshot.Revision)
	return map[string]any{"status": "ok", "revision": snapshot.Revision, "collections": len(specs), "changes": changed}, nil
}

func listJAVMedia(ctx context.Context, host *runtimehost.Client) ([]runtimehost.CatalogMediaItem, error) {
	var items []runtimehost.CatalogMediaItem
	token := ""
	for {
		resp, err := host.ListLibraryMedia(ctx, runtimehost.ListLibraryMediaRequest{PageSize: 200, PageToken: token})
		if err != nil {
			return nil, fmt.Errorf("list library media: %w", err)
		}
		for _, item := range resp.Items {
			if item.ExternalProvider == capabilityID {
				items = append(items, item)
			}
		}
		if resp.NextPageToken == "" {
			break
		}
		token = resp.NextPageToken
	}
	return items, nil
}

func collectionSpecs(snapshot *provider.LibrarySync, media []runtimehost.CatalogMediaItem) []provider.CollectionSpec {
	if snapshot == nil {
		return nil
	}
	byRelease := map[string]map[int64][]string{}
	byScene := map[string]map[string][]string{}
	libraries := map[string]bool{}
	for _, item := range media {
		if item.MediaID == "" || item.LibraryID == "" {
			continue
		}
		libraries[item.LibraryID] = true
		if byRelease[item.LibraryID] == nil {
			byRelease[item.LibraryID] = map[int64][]string{}
			byScene[item.LibraryID] = map[string][]string{}
		}
		if id, err := strconv.ParseInt(item.ExternalID, 10, 64); err == nil && id > 0 {
			byRelease[item.LibraryID][id] = append(byRelease[item.LibraryID][id], item.MediaID)
		}
		if strings.HasPrefix(item.ExternalID, "stash:") {
			byScene[item.LibraryID][strings.TrimPrefix(item.ExternalID, "stash:")] = append(byScene[item.LibraryID][strings.TrimPrefix(item.ExternalID, "stash:")], item.MediaID)
		}
	}
	var specs []provider.CollectionSpec
	for lib := range libraries {
		seen := map[string]bool{}
		watch := []string{}
		for _, entry := range snapshot.Watchlist {
			ids := byRelease[lib][entry.ReleaseID]
			if len(ids) == 0 && entry.StashSceneID != "" {
				ids = byScene[lib][entry.StashSceneID]
			}
			for _, id := range ids {
				if !seen[id] {
					watch = append(watch, id)
					seen[id] = true
				}
			}
		}
		specs = append(specs, provider.CollectionSpec{Kind: "watchlist", Name: "Watchlist", LibraryID: lib, MediaIDs: watch})
		for _, preset := range snapshot.FilterPresets {
			seen = map[string]bool{}
			ids := []string{}
			for _, releaseID := range preset.ReleaseIDs {
				for _, id := range byRelease[lib][releaseID] {
					if !seen[id] {
						ids = append(ids, id)
						seen[id] = true
					}
				}
			}
			specs = append(specs, provider.CollectionSpec{Kind: "preset", PresetID: preset.ID, Name: preset.Name, LibraryID: lib, MediaIDs: ids})
		}
	}
	return specs
}
