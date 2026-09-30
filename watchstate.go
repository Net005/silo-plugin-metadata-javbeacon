package main

import (
	"context"
	"sort"
	"strings"
	"time"

	"github.com/Net005/silo-plugin-metadata-javbeacon/provider"
)

// pollWatched runs independently of collection writes. A stalled collection
// must not postpone Stash watched-state import in another movie library.
func (s *collectionSyncTaskServer) pollWatched() {
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()
	for {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
		changed, complete, err := s.syncWatched(ctx)
		cancel()
		if err != nil {
			s.logger().Warn("Stash watched-state sync failed", "error", err)
		} else if changed > 0 {
			s.logger().Info("Stash watched state imported", "items", changed, "complete", complete)
		}
		<-ticker.C
	}
}

func sceneBelongsToLibrary(path string, library provider.MovieLibrary) bool {
	for _, root := range library.Paths {
		root = strings.TrimRight(root, "/")
		if root != "" && (path == root || strings.HasPrefix(path, root+"/")) {
			return true
		}
	}
	return false
}

func (s *collectionSyncTaskServer) syncWatched(ctx context.Context) (int, bool, error) {
	p := s.runtime.provider
	if !p.Configured() || p.SiloAPIKey() == "" || p.SiloBaseURL() == "" {
		return 0, true, nil
	}
	snapshot, err := p.LibrarySync(ctx)
	if err != nil {
		return 0, false, err
	}
	if snapshot == nil || len(snapshot.Watched) == 0 {
		return 0, true, nil
	}
	client := provider.NewSiloClient(p.SiloBaseURL(), p.SiloAPIKey())
	libraries, err := client.ListMovieLibraries(ctx)
	if err != nil {
		return 0, false, err
	}
	// Clear the smaller libraries first so a large JAV catch-up cannot
	// starve Hentaied or Other during the initial import.
	counts := map[string]int{}
	for _, library := range libraries {
		for _, item := range snapshot.Watched {
			if sceneBelongsToLibrary(item.Path, library) {
				counts[library.ID]++
			}
		}
	}
	sort.SliceStable(libraries, func(i, j int) bool { return counts[libraries[i].ID] < counts[libraries[j].ID] })
	changed := 0
	for _, library := range libraries {
		watched := make([]provider.LibrarySyncItem, 0)
		for _, item := range snapshot.Watched {
			if item.Path != "" && sceneBelongsToLibrary(item.Path, library) {
				watched = append(watched, item)
			}
		}
		if len(watched) == 0 {
			continue
		}
		catalog, profileID, err := client.ListLibraryCatalogForProfile(ctx, library.ID)
		if err != nil {
			return changed, false, err
		}
		n, complete, err := syncWatchedCatalog(ctx, client, profileID, &provider.LibrarySync{Watched: watched}, catalog, snapshot.ReleaseCodes, 200-changed)
		changed += n
		if err != nil || !complete {
			return changed, false, err
		}
		if changed >= 200 {
			return changed, false, nil
		}
	}
	return changed, true, nil
}
