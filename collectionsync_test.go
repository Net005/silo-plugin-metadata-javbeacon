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

func TestCollectionSourceFingerprintTracksMembershipButNotWatchedHistory(t *testing.T) {
	snapshot := &provider.LibrarySync{
		Watchlist:     []provider.LibrarySyncItem{{ReleaseID: 1}},
		Watched:       []provider.LibrarySyncItem{{ReleaseID: 9}},
		FilterPresets: []provider.FilterPresetCollection{{ID: 4, ReleaseIDs: []int64{1}}},
		ReleaseCodes:  map[int64]string{1: "ABC-1"},
	}
	filters := []provider.StashSavedFilter{{ID: "3", Items: []provider.StashSavedFilterItem{{SceneID: "8", Path: "/stash/one.mp4"}}}}
	first, err := collectionSourceFingerprint(snapshot, filters, "Watchlist", "Stash | ", "", "")
	if err != nil {
		t.Fatal(err)
	}
	snapshot.Watched = append(snapshot.Watched, provider.LibrarySyncItem{ReleaseID: 10})
	unchanged, err := collectionSourceFingerprint(snapshot, filters, "Watchlist", "Stash | ", "", "")
	if err != nil || unchanged != first {
		t.Fatalf("watched history changed fingerprint: %q %v", unchanged, err)
	}
	filters[0].Items[0].Path = "/stash/two.mp4"
	changed, err := collectionSourceFingerprint(snapshot, filters, "Watchlist", "Stash | ", "", "")
	if err != nil || changed == first {
		t.Fatalf("Stash membership did not change fingerprint: %q %v", changed, err)
	}
	snapshot.Watchlist = append(snapshot.Watchlist, provider.LibrarySyncItem{ReleaseID: 2})
	changedAgain, err := collectionSourceFingerprint(snapshot, filters, "Watchlist", "Stash | ", "", "")
	if err != nil || changedAgain == changed {
		t.Fatalf("Watchlist membership did not change fingerprint: %q %v", changedAgain, err)
	}
}

func TestCanonicalTaskKey(t *testing.T) {
	for input, want := range map[string]string{"match-unmatched": "match-unmatched", "plugin:5:match-unmatched": "match-unmatched", "collection-sync": "collection-sync", "plugin:5:collection-sync": "collection-sync", "repair-matched": "repair-matched", "plugin:5:repair-matched": "repair-matched", "metadata-refresh": "metadata-refresh", "watched-sync": "watched-sync", "play-backfill": "play-backfill", "plugin:5:play-backfill": "play-backfill"} {
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

func TestWatchedCatalogMatchesStashOnlySceneTitle(t *testing.T) {
	var paths []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		paths = append(paths, r.URL.Path)
		w.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()
	catalog := []provider.CatalogItem{{ContentID: "stash-only", Title: "Washing Time", Type: "movie"}, {ContentID: "other", Title: "Other", Type: "movie"}}
	snapshot := &provider.LibrarySync{Watched: []provider.LibrarySyncItem{{StashSceneID: "27456", Path: "/media/Futanari - 2022-10-14 - Washing Time [WEBDL-2160p].mp4", Title: "Washing Time", PlayCount: 2}}}
	changed, complete, err := syncWatchedCatalog(context.Background(), provider.NewSiloClient(server.URL, "key"), "profile", snapshot, catalog, nil, 400)
	if err != nil || !complete || changed != 1 || len(paths) != 1 || paths[0] != "/api/v2/watched/stash-only" {
		t.Fatalf("changed=%d complete=%v paths=%v err=%v", changed, complete, paths, err)
	}
}

func TestWatchedCatalogSkipsAmbiguousStashTitle(t *testing.T) {
	var called bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { called = true; w.WriteHeader(http.StatusNoContent) }))
	defer server.Close()
	catalog := []provider.CatalogItem{{ContentID: "one", Title: "Same Title", Type: "movie"}, {ContentID: "two", Title: "Same Title", Type: "movie"}}
	snapshot := &provider.LibrarySync{Watched: []provider.LibrarySyncItem{{StashSceneID: "1", Title: "Same Title"}}}
	changed, _, err := syncWatchedCatalog(context.Background(), provider.NewSiloClient(server.URL, "key"), "profile", snapshot, catalog, nil, 400)
	if err != nil || changed != 0 || called {
		t.Fatalf("changed=%d called=%v err=%v", changed, called, err)
	}
}

func TestWatchedCatalogUsesExactStashPathWhenTitleDiffers(t *testing.T) {
	path := "/collections/hentaied/Hentaied/Hentaied - 2021-10-30 - Agatha Vega [WEBDL-2160p].mp4"
	id := siloLocalContentID(path)
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/api/v2/watched/"+id {
			t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
		}
		calls++
		w.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()
	catalog := []provider.CatalogItem{{ContentID: id, Title: "Completely different Silo title", Type: "movie"}}
	snapshot := &provider.LibrarySync{Watched: []provider.LibrarySyncItem{{StashSceneID: "1056", Path: path, Title: "Agatha Vega", PlayCount: 1}}}
	changed, complete, err := syncWatchedCatalog(context.Background(), provider.NewSiloClient(server.URL, "key"), "profile", snapshot, catalog, nil, 400)
	if err != nil || !complete || changed != 1 || calls != 1 {
		t.Fatalf("changed=%d complete=%v calls=%d err=%v", changed, complete, calls, err)
	}
}
