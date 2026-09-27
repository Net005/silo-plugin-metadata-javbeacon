package main

import (
	"context"
	"github.com/Net005/silo-plugin-metadata-javbeacon/provider"
	"github.com/Silo-Server/silo-plugin-sdk/pkg/pluginsdk/runtimehost"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestCollectionSpecsPreserveSourceOrderAndStashFallback(t *testing.T) {
	snapshot := &provider.LibrarySync{Watchlist: []provider.LibrarySyncItem{{ReleaseID: 2}, {StashSceneID: "scene"}, {ReleaseID: 1}}, FilterPresets: []provider.FilterPresetCollection{{ID: 3, Name: "Favorites", ReleaseIDs: []int64{1, 2}}}}
	media := []runtimehost.CatalogMediaItem{{MediaID: "one", LibraryID: "lib", ExternalProvider: "javbeacon", ExternalID: "1"}, {MediaID: "two", LibraryID: "lib", ExternalProvider: "javbeacon", ExternalID: "2"}, {MediaID: "three", LibraryID: "lib", ExternalProvider: "javbeacon", ExternalID: "stash:scene"}}
	specs := collectionSpecs(snapshot, media)
	if len(specs) != 2 {
		t.Fatalf("specs=%v", specs)
	}
	if specs[0].Kind != "watchlist" || len(specs[0].MediaIDs) != 3 || specs[0].MediaIDs[0] != "two" || specs[0].MediaIDs[1] != "three" || specs[0].MediaIDs[2] != "one" {
		t.Fatalf("watchlist=%v", specs[0])
	}
	if specs[1].Kind != "preset" || len(specs[1].MediaIDs) != 2 || specs[1].MediaIDs[0] != "one" || specs[1].MediaIDs[1] != "two" {
		t.Fatalf("preset=%v", specs[1])
	}
}

func TestCanonicalTaskKey(t *testing.T) {
	for input, want := range map[string]string{"match-unmatched": "match-unmatched", "plugin:5:match-unmatched": "match-unmatched", "collection-sync": "collection-sync", "plugin:5:collection-sync": "collection-sync"} {
		if got := canonicalTaskKey(input); got != want {
			t.Errorf("%q => %q, want %q", input, got, want)
		}
	}
}

func TestCatalogSpecsUseLocalItemsAndPreserveOrder(t *testing.T) {
	snapshot := &provider.LibrarySync{Watchlist: []provider.LibrarySyncItem{{Path: "/stash/SSNI-675.mp4"}, {Path: "/stash/abgd-01.wmv"}, {Path: "/stash/missing.mp4"}}}
	catalog := []provider.CatalogItem{{ContentID: "abgd", Title: "ABGD-1", Type: "movie"}, {ContentID: "ssni", Title: "SSNI-675", Type: "movie"}, {ContentID: "other", Title: "Else", Type: "movie"}}
	specs := collectionSpecsFromCatalog(snapshot, catalog, nil, "16")
	if len(specs) != 1 || len(specs[0].MediaIDs) != 2 || specs[0].MediaIDs[0] != "ssni" || specs[0].MediaIDs[1] != "abgd" {
		t.Fatalf("specs=%v", specs)
	}
}

func TestCatalogSpecsUseSnapshotReleaseCodesForPresetMembers(t *testing.T) {
	snapshot := &provider.LibrarySync{FilterPresets: []provider.FilterPresetCollection{{ID: 7, Name: "Debt", ReleaseIDs: []int64{12, 11}}}}
	catalog := []provider.CatalogItem{{ContentID: "first", Title: "CODE-11", Type: "movie"}, {ContentID: "second", Title: "CODE-12", Type: "movie"}}
	codes := map[int64]string{11: "CODE-11", 12: "CODE-12"}
	specs := collectionSpecsFromCatalog(snapshot, catalog, codes, "16")
	if len(specs) != 2 || len(specs[1].MediaIDs) != 2 || specs[1].MediaIDs[0] != "second" || specs[1].MediaIDs[1] != "first" {
		t.Fatalf("preset order and membership: %+v", specs)
	}
}

func TestWatchedCatalogAppliesOnlyLocalUnplayedMatches(t *testing.T) {
	var paths []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Profile-Id") != "profile" {
			t.Errorf("missing profile header")
		}
		paths = append(paths, r.URL.Path)
		w.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()
	catalog := []provider.CatalogItem{{ContentID: "one", Title: "ABC-1", Type: "movie"}, {ContentID: "two", Title: "ABC-2", Type: "movie"}, {ContentID: "three", Title: "ABC-3", Type: "movie"}}
	catalog[1].UserState.Played = true
	snapshot := &provider.LibrarySync{Watched: []provider.LibrarySyncItem{{ReleaseID: 1}, {ReleaseID: 2}, {ReleaseID: 4}}}
	changed, complete, err := syncWatchedCatalog(context.Background(), provider.NewSiloClient(server.URL, "key"), "profile", snapshot, catalog, map[int64]string{1: "ABC-1", 2: "ABC-2", 4: "ABC-4"}, 400)
	if err != nil || !complete || changed != 1 || len(paths) != 1 || paths[0] != "/api/v2/watched/one" {
		t.Fatalf("changed=%d complete=%v paths=%v err=%v", changed, complete, paths, err)
	}
}

func TestCatalogWatchlistUsesReleaseCodeWhenPathMissing(t *testing.T) {
	snapshot := &provider.LibrarySync{Watchlist: []provider.LibrarySyncItem{{ReleaseID: 7}}}
	catalog := []provider.CatalogItem{{ContentID: "local", Title: "ABC-7", Type: "movie"}}
	specs := collectionSpecsFromCatalog(snapshot, catalog, map[int64]string{7: "ABC-7"}, "16")
	if len(specs) != 1 || len(specs[0].MediaIDs) != 1 || specs[0].MediaIDs[0] != "local" {
		t.Fatalf("watchlist fallback: %+v", specs)
	}
}
