package main

import (
	"context"
	"fmt"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/hashicorp/go-hclog"

	pluginv1 "github.com/Silo-Server/silo-plugin-sdk/pkg/pluginproto/silo/plugin/v1"
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
	log             hclog.Logger
	mu              sync.Mutex
	running         map[string]bool
	matchCursor     string
	matchOffset     int
	metadataPending *metadataRefreshBatch
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

func canonicalTaskKey(key string) string {
	if key == "match-unmatched" || strings.HasSuffix(key, ":match-unmatched") {
		return "match-unmatched"
	}
	return "collection-sync"
}

// Run dispatches on task_key, since Silo's plugin SDK exposes only one
// ScheduledTask service per plugin process - the manifest declares each
// scheduled_task.v1 capability as a separate id, and Silo passes that id back
// as task_key on every Run call. Anything other than "match-unmatched" (an
// empty key, or the id "collection-sync") runs collection reconciliation.
type collectionSyncResult struct {
	summary map[string]any
	err     error
}

func (s *collectionSyncTaskServer) startCollectionSync() (<-chan collectionSyncResult, bool) {
	s.mu.Lock()
	if s.running == nil {
		s.running = make(map[string]bool)
	}
	if s.running["collection-sync"] {
		s.mu.Unlock()
		return nil, false
	}
	s.running["collection-sync"] = true
	s.mu.Unlock()
	done := make(chan collectionSyncResult, 1)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
		defer cancel()
		summary, err := s.sync(ctx)
		if err != nil {
			s.logger().Error("collection sync failed", "err", err)
		}
		done <- collectionSyncResult{summary, err}
		s.mu.Lock()
		delete(s.running, "collection-sync")
		s.mu.Unlock()
	}()
	return done, true
}

// Silo's scheduled-task RPC has a hard ten-second control deadline even when
// a trigger advertises thirty seconds. Let the resident worker finish the
// sync and return its result when quick; otherwise report that it continues.
func (s *collectionSyncTaskServer) Run(ctx context.Context, req *pluginv1.RunScheduledTaskRequest) (*pluginv1.RunScheduledTaskResponse, error) {
	taskKey := canonicalTaskKey(req.GetTaskKey())
	if taskKey == "collection-sync" {
		done, started := s.startCollectionSync()
		if !started {
			return taskOutput(map[string]any{"status": "running"})
		}
		timer := time.NewTimer(8 * time.Second)
		defer timer.Stop()
		select {
		case result := <-done:
			if result.err != nil {
				return nil, result.err
			}
			return taskOutput(result.summary)
		case <-timer.C:
			return taskOutput(map[string]any{"status": "running", "detail": "Collection and watched-state sync continues in the background"})
		case <-ctx.Done():
			return taskOutput(map[string]any{"status": "running"})
		}
	}
	done, started := s.startMatchSync()
	if !started {
		return taskOutput(map[string]any{"status": "running"})
	}
	timer := time.NewTimer(8 * time.Second)
	defer timer.Stop()
	select {
	case result := <-done:
		if result.err != nil {
			return nil, result.err
		}
		return taskOutput(result.summary)
	case <-timer.C:
		return taskOutput(map[string]any{"status": "running", "detail": "Auto-match continues in the background"})
	case <-ctx.Done():
		return taskOutput(map[string]any{"status": "running"})
	}

}

func (s *collectionSyncTaskServer) startMatchSync() (<-chan collectionSyncResult, bool) {
	s.mu.Lock()
	if s.running == nil {
		s.running = make(map[string]bool)
	}
	if s.running["match-unmatched"] {
		s.mu.Unlock()
		return nil, false
	}
	s.running["match-unmatched"] = true
	s.mu.Unlock()
	done := make(chan collectionSyncResult, 1)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
		defer cancel()
		summary, err := s.matchUnmatched(ctx)
		if err != nil {
			s.logger().Error("auto-match failed", "error", err)
		}
		done <- collectionSyncResult{summary, err}
		s.mu.Lock()
		delete(s.running, "match-unmatched")
		s.mu.Unlock()
	}()
	return done, true
}

func (s *collectionSyncTaskServer) pollMatch() {
	ticker := time.NewTicker(time.Minute)
	defer ticker.Stop()
	for {
		s.startMatchSync()
		<-ticker.C
	}
}

func taskOutput(summary map[string]any) (*pluginv1.RunScheduledTaskResponse, error) {
	output, err := structpb.NewStruct(summary)
	if err != nil {
		return nil, err
	}
	return &pluginv1.RunScheduledTaskResponse{Output: output}, nil
}

func (s *collectionSyncTaskServer) poll() {
	ticker := time.NewTicker(time.Minute)
	defer ticker.Stop()
	for range ticker.C {
		s.startCollectionSync()
	}
}

func (s *collectionSyncTaskServer) sync(ctx context.Context) (map[string]any, error) {
	snapshot, err := s.runtime.provider.LibrarySync(ctx)
	if err != nil {
		return nil, err
	}
	key := s.runtime.provider.SiloAPIKey()
	if key == "" {
		return map[string]any{"status": "skipped", "reason": "Silo API key is not configured"}, nil
	}
	baseURL := s.runtime.provider.SiloBaseURL()
	if baseURL == "" {
		return nil, fmt.Errorf("collection-sync: Silo URL is required")
	}
	client := provider.NewSiloClient(baseURL, key)
	stashSelection, stashPrefix, javSelection, javPrefix := s.runtime.provider.SavedFilterSettings()
	snapshot, err = selectedFilterSnapshot(snapshot, javSelection, javPrefix)
	if err != nil {
		return nil, err
	}
	stashFilters := []provider.StashSavedFilter{}
	if strings.TrimSpace(stashSelection) != "" {
		stashFilters, err = s.runtime.provider.StashSavedFilters(ctx, stashSelection)
		if err != nil {
			return nil, fmt.Errorf("Stash saved filters: %w", err)
		}
	}
	libraries, err := client.ListJAVBeaconMovieLibraries(ctx)
	if err != nil {
		return nil, err
	}
	codes := snapshot.ReleaseCodes
	if codes == nil {
		codes, err = s.runtime.provider.LocalReleaseCodes(ctx)
		if err != nil {
			return nil, err
		}
	}
	var specs []provider.CollectionSpec
	for _, library := range libraries {
		catalog, _, err := client.ListLibraryCatalogForProfile(ctx, library.ID)
		if err != nil {
			return nil, fmt.Errorf("collection-sync: library %s: %w", library.ID, err)
		}
		// The scope entry lets reconciliation remove stale owned collections
		// even when every filter has zero local matches in this library.
		specs = append(specs, provider.CollectionSpec{Kind: "library", LibraryID: library.ID})
		for _, spec := range collectionSpecsFromCatalog(snapshot, catalog, codes, library.ID) {
			if len(spec.MediaIDs) > 0 {
				specs = append(specs, spec)
			}
		}
		for _, spec := range stashFilterSpecsFromCatalog(stashFilters, catalog, library.ID, stashPrefix) {
			if len(spec.MediaIDs) > 0 {
				specs = append(specs, spec)
			}
		}
	}
	changed, complete, err := client.SyncCollectionsBatch(ctx, specs, 400, strings.TrimSpace(javSelection) != "", true, true)
	if err != nil {
		if strings.Contains(err.Error(), "HTTP 429") {
			return map[string]any{"status": "partial", "reason": "rate_limited", "changes": changed, "watched_changes": 0}, nil
		}
		return map[string]any{"status": "error", "error": err.Error()}, err
	}
	if !complete {
		return map[string]any{"status": "partial", "reason": "batch_limit", "collections": len(specs), "changes": changed, "watched_changes": 0}, nil
	}
	s.runtime.provider.SetLastSyncedRevision(snapshot.Revision)
	return map[string]any{"status": "ok", "revision": snapshot.Revision, "collections": len(specs), "changes": changed, "watched_changes": 0}, nil
}

func syncItemCode(entry provider.LibrarySyncItem, codes map[int64]string) string {
	if strings.TrimSpace(entry.Path) != "" {
		code := strings.TrimSuffix(filepath.Base(entry.Path), filepath.Ext(entry.Path))
		if code != "" && code != "." {
			return code
		}
	}
	return codes[entry.ReleaseID]
}

// Silo's generic watch-provider importer only accepts TMDB/IMDb/TVDB IDs.
// JAV releases have none, so apply the authoritative watched flag to local
// catalog members directly. Existing played flags prevent repeated writes.
func syncWatchedCatalog(ctx context.Context, client *provider.SiloClient, profileID string, snapshot *provider.LibrarySync, catalog []provider.CatalogItem, codes map[int64]string, limit int) (int, bool, error) {
	// Resolve only unique catalog titles. Stash-only items may display either
	// the Stash scene title or their original filename until metadata refresh.
	byTitle := map[string][]string{}
	for _, item := range catalog {
		if item.Type == "movie" && item.ContentID != "" {
			key := normalizedCatalogCode(item.Title)
			byTitle[key] = append(byTitle[key], item.ContentID)
		}
	}
	wanted := map[string]bool{}
	catalogIDs := map[string]bool{}
	for _, item := range catalog {
		if item.Type == "movie" && item.ContentID != "" {
			catalogIDs[item.ContentID] = true
		}
	}
	for _, entry := range snapshot.Watched {
		if entry.Path != "" {
			id := siloLocalContentID(entry.Path)
			if catalogIDs[id] {
				wanted[id] = true
				continue
			}
		}
		keys := []string{syncItemCode(entry, codes), entry.Title}
		for _, candidate := range keys {
			if candidate == "" {
				continue
			}
			matches := byTitle[normalizedCatalogCode(candidate)]
			if len(matches) == 1 {
				wanted[matches[0]] = true
				break
			}
		}
	}
	changed := 0
	for _, item := range catalog {
		if item.Type != "movie" || item.ContentID == "" || item.UserState.Played || !wanted[item.ContentID] {
			continue
		}
		if limit > 0 && changed >= limit {
			return changed, false, nil
		}
		if err := client.MarkWatched(ctx, profileID, item.ContentID); err != nil {
			if strings.Contains(err.Error(), "HTTP 429") {
				return changed, false, nil
			}
			return changed, false, err
		}
		changed++
	}
	return changed, true, nil
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

// stashFilterSpecsFromCatalog uses the exact Stash file path to resolve a
// local Silo content ID. A scene outside this library is never imported.
func stashFilterSpecsFromCatalog(filters []provider.StashSavedFilter, catalog []provider.CatalogItem, libraryID, prefix string) []provider.CollectionSpec {
	byID := map[string]provider.CatalogItem{}
	for _, item := range catalog {
		if item.Type == "movie" {
			byID[item.ContentID] = item
		}
	}
	specs := make([]provider.CollectionSpec, 0, len(filters))
	for _, filter := range filters {
		spec := provider.CollectionSpec{Kind: "stash_preset", PresetKey: filter.ID, Name: prefix + filter.Name, LibraryID: libraryID}
		seen := map[string]bool{}
		for _, entry := range filter.Items {
			id := siloLocalContentID(entry.Path)
			if item, ok := byID[id]; ok && !seen[id] {
				spec.MediaIDs = append(spec.MediaIDs, id)
				seen[id] = true
				spec.Artwork = append(spec.Artwork, provider.CollectionArtwork{MediaID: id, PosterURL: item.PosterURL, BackdropURL: item.BackdropURL, ReleaseDate: item.ReleaseDate, AddedAt: item.AddedAt})
			}
		}
		specs = append(specs, spec)
	}
	return specs
}

func stashFilterSpecsFromMedia(filters []provider.StashSavedFilter, media []runtimehost.CatalogMediaItem, prefix string) []provider.CollectionSpec {
	byScene := map[string]map[string][]string{}
	for _, item := range media {
		if item.MediaID == "" || item.LibraryID == "" || !strings.HasPrefix(item.ExternalID, "stash:") {
			continue
		}
		if byScene[item.LibraryID] == nil {
			byScene[item.LibraryID] = map[string][]string{}
		}
		sceneID := strings.TrimPrefix(item.ExternalID, "stash:")
		byScene[item.LibraryID][sceneID] = append(byScene[item.LibraryID][sceneID], item.MediaID)
	}
	specs := []provider.CollectionSpec{}
	for lib, scenes := range byScene {
		for _, filter := range filters {
			spec := provider.CollectionSpec{Kind: "stash_preset", PresetKey: filter.ID, Name: prefix + filter.Name, LibraryID: lib}
			seen := map[string]bool{}
			for _, entry := range filter.Items {
				for _, id := range scenes[entry.SceneID] {
					if !seen[id] {
						spec.MediaIDs = append(spec.MediaIDs, id)
						seen[id] = true
					}
				}
			}
			specs = append(specs, spec)
		}
	}
	return specs
}

// selectedFilterSnapshot applies exact saved-filter names or numeric IDs. A
// blank selection keeps all presets; an unmatched nonblank selection imports
// none, avoiding an accidental broad import after a typo.
func selectedFilterSnapshot(snapshot *provider.LibrarySync, selection, prefix string) (*provider.LibrarySync, error) {
	if snapshot == nil {
		return nil, nil
	}
	copy := *snapshot
	selected := map[string]bool{}
	found := map[string]bool{}
	for _, token := range strings.FieldsFunc(selection, func(r rune) bool { return r == ',' || r == '\n' }) {
		if token = strings.ToLower(strings.TrimSpace(token)); token != "" {
			selected[token] = true
		}
	}
	copy.FilterPresets = make([]provider.FilterPresetCollection, 0, len(snapshot.FilterPresets))
	for _, preset := range snapshot.FilterPresets {
		if len(selected) > 0 && !selected[strconv.FormatInt(preset.ID, 10)] && !selected[strings.ToLower(strings.TrimSpace(preset.Name))] {
			continue
		}
		found[strconv.FormatInt(preset.ID, 10)] = true
		found[strings.ToLower(strings.TrimSpace(preset.Name))] = true
		preset.Name = prefix + preset.Name
		copy.FilterPresets = append(copy.FilterPresets, preset)
	}
	for token := range selected {
		if !found[token] {
			return nil, fmt.Errorf("saved filter %q was not found; check its exact name or numeric ID", token)
		}
	}
	return &copy, nil
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

var siloCodePattern = regexp.MustCompile(`(?i)^([a-z]{2,12})[-_ ]*0*([0-9]{1,6})$`)

func normalizedCatalogCode(raw string) string {
	name := strings.TrimSpace(raw)
	if match := siloCodePattern.FindStringSubmatch(name); len(match) == 3 {
		number, _ := strconv.Atoi(match[2])
		return strings.ToUpper(match[1]) + "-" + strconv.Itoa(number)
	}
	return strings.ToLower(name)
}

// collectionSpecsFromCatalog maps JAVBeacon's ordered source snapshot to
// items that actually exist in the configured Silo library. Unmatched local
// items remain eligible; the catalog is the source of local existence.
func collectionSpecsFromCatalog(snapshot *provider.LibrarySync, catalog []provider.CatalogItem, codes map[int64]string, libraryID string) []provider.CollectionSpec {
	byCode := map[string][]string{}
	for _, item := range catalog {
		if item.ContentID == "" || item.Type != "movie" {
			continue
		}
		code := normalizedCatalogCode(item.Title)
		byCode[code] = append(byCode[code], item.ContentID)
	}
	appendUnique := func(dst []string, seen map[string]bool, code string) []string {
		for _, id := range byCode[normalizedCatalogCode(code)] {
			if !seen[id] {
				dst = append(dst, id)
				seen[id] = true
			}
		}
		return dst
	}
	watch := []string{}
	seen := map[string]bool{}
	for _, entry := range snapshot.Watchlist {
		code := syncItemCode(entry, codes)
		watch = appendUnique(watch, seen, code)
	}
	specs := []provider.CollectionSpec{{Kind: "watchlist", Name: "Watchlist", LibraryID: libraryID, MediaIDs: watch}}
	for _, preset := range snapshot.FilterPresets {
		ids := []string{}
		seen = map[string]bool{}
		for _, releaseID := range preset.ReleaseIDs {
			ids = appendUnique(ids, seen, codes[releaseID])
		}
		specs = append(specs, provider.CollectionSpec{Kind: "preset", PresetID: preset.ID, Name: preset.Name, LibraryID: libraryID, MediaIDs: ids})
	}
	artByID := make(map[string]provider.CollectionArtwork, len(catalog))
	for _, item := range catalog {
		artByID[item.ContentID] = provider.CollectionArtwork{MediaID: item.ContentID, PosterURL: item.PosterURL, BackdropURL: item.BackdropURL, ReleaseDate: item.ReleaseDate, AddedAt: item.AddedAt}
	}
	for i := range specs {
		for _, id := range specs[i].MediaIDs {
			if art, ok := artByID[id]; ok {
				specs[i].Artwork = append(specs[i].Artwork, art)
			}
		}
	}
	return specs
}
