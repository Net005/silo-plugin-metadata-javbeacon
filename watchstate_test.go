package main

import (
	"github.com/Net005/silo-plugin-metadata-javbeacon/provider"
	"testing"
)

func TestSceneBelongsToOnlyItsMovieLibrary(t *testing.T) {
	hentaied := provider.MovieLibrary{ID: "18", Paths: []string{"/collections/hentaied/Hentaied"}}
	other := provider.MovieLibrary{ID: "19", Paths: []string{"/collections/misc/other"}}
	jav := provider.MovieLibrary{ID: "16", Paths: []string{"/collections/giga", "/collections/jav"}}
	path := "/collections/hentaied/Hentaied/Hentaied - 2021-10-30 - Agatha Vega.mp4"
	if !sceneBelongsToLibrary(path, hentaied) || sceneBelongsToLibrary(path, other) || sceneBelongsToLibrary(path, jav) {
		t.Fatal("watched scene routed to wrong library")
	}
	if !sceneBelongsToLibrary("/collections/misc/other/file.mp4", other) {
		t.Fatal("Other library path not matched")
	}
}
