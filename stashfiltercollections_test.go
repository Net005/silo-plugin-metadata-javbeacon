package main

import (
	"github.com/Net005/silo-plugin-metadata-javbeacon/provider"
	"testing"
)

func TestStashFilterSpecsUseExactLocalPathAndSeparatePrefix(t *testing.T) {
	path := "/collections/other/scene.mkv"
	id := siloLocalContentID(path)
	filters := []provider.StashSavedFilter{{ID: "7", Name: "Favorites", Items: []provider.StashSavedFilterItem{{SceneID: "42", Path: path}, {SceneID: "99", Path: "/missing.mkv"}, {SceneID: "42", Path: path}}}}
	catalog := []provider.CatalogItem{{ContentID: id, Title: "Local", Type: "movie", PosterURL: "/poster"}}
	specs := stashFilterSpecsFromCatalog(filters, catalog, "other", "Stash | ")
	if len(specs) != 1 || specs[0].Kind != "stash_preset" || specs[0].Name != "Stash | Favorites" || specs[0].PresetKey != "7" || len(specs[0].MediaIDs) != 1 || specs[0].MediaIDs[0] != id || len(specs[0].Artwork) != 1 {
		t.Fatalf("specs=%+v", specs)
	}
}
