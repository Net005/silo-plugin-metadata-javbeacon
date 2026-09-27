package main

import (
	"testing"

	"github.com/Net005/silo-plugin-metadata-javbeacon/provider"
)

func TestCollectionReleaseIDsIncludesWatchlistWithoutPresets(t *testing.T) {
	ids := collectionReleaseIDs(&provider.LibrarySync{Watchlist: []provider.LibrarySyncItem{{ReleaseID: 42}}, FilterPresets: []provider.FilterPresetCollection{{ReleaseIDs: []int64{42, 43}}}})
	if len(ids) != 2 || !ids[42] || !ids[43] {
		t.Fatalf("derived-tag release IDs = %v", ids)
	}
}
