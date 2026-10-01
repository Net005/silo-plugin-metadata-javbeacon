package main

import (
	"github.com/Net005/silo-plugin-metadata-javbeacon/provider"
	"testing"
)

func TestSelectedFilterSnapshot(t *testing.T) {
	original := &provider.LibrarySync{FilterPresets: []provider.FilterPresetCollection{
		{ID: 1, Name: "Prison"}, {ID: 2, Name: "Office Lady"}, {ID: 3, Name: "Other"},
	}}
	selected, err := selectedFilterSnapshot(original, "PRISON, 2", "Stash | ")
	if err != nil {
		t.Fatal(err)
	}
	if len(selected.FilterPresets) != 2 || selected.FilterPresets[0].Name != "Stash | Prison" || selected.FilterPresets[1].Name != "Stash | Office Lady" {
		t.Fatalf("selection: %+v", selected.FilterPresets)
	}
	if original.FilterPresets[0].Name != "Prison" {
		t.Fatal("mutated source snapshot")
	}
	if _, err := selectedFilterSnapshot(original, "unknown", ""); err == nil {
		t.Fatal("unknown selection must not remove existing collections")
	}
	if got, err := selectedFilterSnapshot(original, "", ""); err != nil || len(got.FilterPresets) != 3 {
		t.Fatalf("blank selection: %v, %v", got, err)
	}
}
