package main

import (
	"github.com/Net005/silo-plugin-metadata-javbeacon/provider"
	"github.com/Silo-Server/silo-plugin-sdk/pkg/pluginsdk/runtimehost"
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
