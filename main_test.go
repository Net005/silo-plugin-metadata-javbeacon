package main

import (
	"testing"

	"google.golang.org/protobuf/types/known/structpb"

	"github.com/Net005/silo-plugin-metadata-javbeacon/provider"
)

func TestSupportsItemType(t *testing.T) {
	cases := map[string]bool{
		"":        true,
		"movie":   true,
		"series":  false,
		"episode": false,
	}
	for itemType, want := range cases {
		if got := supportsItemType(itemType); got != want {
			t.Errorf("supportsItemType(%q) = %v, want %v", itemType, got, want)
		}
	}
}

func TestJavbeaconCanonicalPath(t *testing.T) {
	cases := map[string]string{
		"":                           "",
		"/covers/1/jellyfin-primary": "javbeacon://covers/1/jellyfin-primary",
		"covers/1/jellyfin-primary":  "javbeacon://covers/1/jellyfin-primary",
	}
	for in, want := range cases {
		if got := javbeaconCanonicalPath(in); got != want {
			t.Errorf("javbeaconCanonicalPath(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestParseInt64(t *testing.T) {
	cases := map[string]int64{
		"42":   42,
		" 42 ": 42,
		"":     0,
		"nope": 0,
		"-1":   -1,
	}
	for in, want := range cases {
		if got := parseInt64(in); got != want {
			t.Errorf("parseInt64(%q) = %d, want %d", in, got, want)
		}
	}
}

func TestReleaseIDFromProto(t *testing.T) {
	if got := releaseIDFromProto(nil); got != 0 {
		t.Fatalf("releaseIDFromProto(nil) = %d, want 0", got)
	}

	empty, err := structpb.NewStruct(map[string]any{"other": "value"})
	if err != nil {
		t.Fatalf("NewStruct: %v", err)
	}
	if got := releaseIDFromProto(empty); got != 0 {
		t.Fatalf("releaseIDFromProto(no javbeacon key) = %d, want 0", got)
	}

	withID, err := structpb.NewStruct(map[string]any{capabilityID: "123"})
	if err != nil {
		t.Fatalf("NewStruct: %v", err)
	}
	if got := releaseIDFromProto(withID); got != 123 {
		t.Fatalf("releaseIDFromProto = %d, want 123", got)
	}
}

func TestProviderIDsStruct(t *testing.T) {
	item := &provider.Metadata{ReleaseID: 5}
	out := providerIDsStruct(item)
	if got := out.AsMap()[capabilityID]; got != "5" {
		t.Fatalf("providerIDsStruct javbeacon id = %v, want \"5\"", got)
	}
	if _, ok := out.AsMap()["stash"]; ok {
		t.Fatal("did not expect a stash id when StashSceneID is empty")
	}

	item.StashSceneID = "scene-1"
	out = providerIDsStruct(item)
	if got := out.AsMap()["stash"]; got != "scene-1" {
		t.Fatalf("providerIDsStruct stash id = %v, want \"scene-1\"", got)
	}
}

func TestSearchResultFromMetadata(t *testing.T) {
	item := &provider.Metadata{
		ReleaseID:      9,
		Code:           "ABC-123",
		Overview:       "desc",
		CoverPath:      "/covers/9/jellyfin-primary",
		ProductionYear: 2024,
	}
	result := searchResultFromMetadata(item)
	if result.ProviderId != "9" {
		t.Errorf("ProviderId = %q, want \"9\"", result.ProviderId)
	}
	if result.ItemType != "movie" {
		t.Errorf("ItemType = %q, want movie", result.ItemType)
	}
	if result.Title != "ABC-123" {
		t.Errorf("Title = %q, want ABC-123", result.Title)
	}
	if result.ImageUrl != "javbeacon://covers/9/jellyfin-primary" {
		t.Errorf("ImageUrl = %q", result.ImageUrl)
	}
	if result.Year != 2024 {
		t.Errorf("Year = %d, want 2024", result.Year)
	}
}

func TestMetadataItemFromResult(t *testing.T) {
	item := &provider.Metadata{
		ReleaseID:         11,
		Code:              "DEF-456",
		OriginalTitle:     "original",
		Overview:          "desc",
		RuntimeSeconds:    7200,
		Genres:            []string{"drama"},
		PremiereDate:      "2024-01-02",
		CoverPath:         "/covers/11/jellyfin-primary",
		CoverBackdropPath: "/covers/11/backdrop",
		Studio:            "Studio A",
		Performers:        []string{"Actress A", "Actress B"},
		Directors:         []string{"Director A"},
	}
	out := metadataItemFromResult(item)

	if out.ProviderId != "11" {
		t.Errorf("ProviderId = %q, want \"11\"", out.ProviderId)
	}
	if out.Runtime != 120 {
		t.Errorf("Runtime = %d, want 120 (minutes)", out.Runtime)
	}
	if out.PosterPath != "javbeacon://covers/11/jellyfin-primary" {
		t.Errorf("PosterPath = %q", out.PosterPath)
	}
	if out.BackdropPath != "javbeacon://covers/11/backdrop" {
		t.Errorf("BackdropPath = %q", out.BackdropPath)
	}
	if len(out.Studios) != 1 || out.Studios[0] != "Studio A" {
		t.Errorf("Studios = %v", out.Studios)
	}
	if len(out.People) != 3 {
		t.Fatalf("People = %v, want 3 entries", out.People)
	}
	if out.People[0].Kind != "actor" || out.People[0].Name != "Actress A" {
		t.Errorf("People[0] = %+v", out.People[0])
	}
	if out.People[1].Kind != "actor" || out.People[1].Name != "Actress B" {
		t.Errorf("People[1] = %+v", out.People[1])
	}
	if out.People[2].Kind != "director" || out.People[2].Name != "Director A" {
		t.Errorf("People[2] = %+v", out.People[2])
	}
}

func TestMetadataItemFromResultNoStudio(t *testing.T) {
	item := &provider.Metadata{ReleaseID: 1, Code: "X"}
	out := metadataItemFromResult(item)
	if out.Studios != nil {
		t.Errorf("Studios = %v, want nil when Studio is empty", out.Studios)
	}
}

// TestMetadataItemFromResultAddsCollectionAndWatchlistGenres guards the
// genre/tag substitute for the two JAVBeacon-side concepts Silo's plugin SDK
// has no first-class capability for: saved filter sets ("Collection: <name>")
// and Watchlist membership ("Watchlist") - both ride along as plain Genres
// entries since Silo has no collection-management or favorites/watchlist
// marker of its own for this plugin to set instead.
func TestMetadataItemFromResultAddsCollectionAndWatchlistGenres(t *testing.T) {
	item := &provider.Metadata{
		ReleaseID:       1,
		Code:            "X",
		Genres:          []string{"drama"},
		CollectionNames: []string{"My Favorites", "", "Second Set"},
		Watchlist:       true,
	}
	out := metadataItemFromResult(item)
	want := []string{"drama", "Collection: My Favorites", "Collection: Second Set", "Watchlist"}
	if len(out.Genres) != len(want) {
		t.Fatalf("Genres = %v, want %v", out.Genres, want)
	}
	for i, g := range want {
		if out.Genres[i] != g {
			t.Errorf("Genres[%d] = %q, want %q", i, out.Genres[i], g)
		}
	}
}

func TestMetadataItemFromResultOmitsWatchlistGenreWhenNotWatchlisted(t *testing.T) {
	item := &provider.Metadata{ReleaseID: 1, Code: "X", Watchlist: false}
	out := metadataItemFromResult(item)
	for _, g := range out.Genres {
		if g == "Watchlist" {
			t.Fatalf("Genres = %v, must not contain Watchlist when item.Watchlist is false", out.Genres)
		}
	}
}

// TestImagesFromMetadataStashScreenshotRidesAlongside guards the new (as of
// JAVBeacon v1.0.239) StashScreenshotURL gap-fill: it must appear as
// ADDITIONAL poster/backdrop candidates alongside CoverPath/
// CoverBackdropPath/BackdropURLs, never replacing them.
func TestImagesFromMetadataStashScreenshotRidesAlongside(t *testing.T) {
	item := &provider.Metadata{
		ReleaseID:          1,
		Code:               "X",
		CoverPath:          "/covers/1/jellyfin-primary",
		CoverBackdropPath:  "/covers/1/original",
		BackdropURLs:       []string{"/screenshots/1/0"},
		StashScreenshotURL: "/api/v1/integrations/silo/releases/1/stash-cover",
	}
	images := imagesFromMetadata(item)
	var gotPoster, gotBackdropStash int
	stashURL := "javbeacon://api/v1/integrations/silo/releases/1/stash-cover"
	for _, img := range images {
		if img.Url == stashURL {
			if img.Kind == "poster" {
				gotPoster++
			}
			if img.Kind == "backdrop" {
				gotBackdropStash++
			}
		}
	}
	if gotPoster != 1 || gotBackdropStash != 1 {
		t.Fatalf("StashScreenshotURL must appear once as poster and once as backdrop, got poster=%d backdrop=%d in %+v", gotPoster, gotBackdropStash, images)
	}
	if len(images) != 5 {
		t.Fatalf("images = %+v, want 5 entries (cover poster, cover backdrop, 1 screenshot backdrop, stash poster, stash backdrop)", images)
	}
}

func TestImagesFromMetadataOmitsStashScreenshotWhenEmpty(t *testing.T) {
	item := &provider.Metadata{ReleaseID: 1, Code: "X", CoverPath: "/covers/1/jellyfin-primary"}
	images := imagesFromMetadata(item)
	if len(images) != 1 {
		t.Fatalf("images = %+v, want exactly the cover poster", images)
	}
}
