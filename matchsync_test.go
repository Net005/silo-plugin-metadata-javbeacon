package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Net005/silo-plugin-metadata-javbeacon/provider"
)

func TestSelectExactReleaseIDAcceptsSingleExactCaseInsensitiveHit(t *testing.T) {
	results := []provider.Metadata{
		{Code: "ADN-131", ReleaseID: 9001},
		{Code: "ADN-132", ReleaseID: 9002},
	}
	id, ok := selectExactReleaseID(results, "adn-131")
	if !ok || id != 9001 {
		t.Fatalf("selectExactReleaseID = (%d, %v), want (9001, true)", id, ok)
	}
}

func TestSelectExactReleaseIDRejectsNoMatch(t *testing.T) {
	results := []provider.Metadata{{Code: "ADN-132", ReleaseID: 9002}}
	if _, ok := selectExactReleaseID(results, "ADN-131"); ok {
		t.Fatal("expected no match for a title with no exact code hit")
	}
}

func TestSelectExactReleaseIDChoosesStableDuplicate(t *testing.T) {
	// Same exact code and equally complete metadata use a stable ID tie-break.
	results := []provider.Metadata{
		{Code: "ADN-131", ReleaseID: 9001},
		{Code: "ADN-131", ReleaseID: 9099},
	}
	if id, ok := selectExactReleaseID(results, "ADN-131"); !ok || id != 9001 {
		t.Fatalf("duplicate id=%d ok=%v", id, ok)
	}
}

// TestSelectExactReleaseIDPicksMostCompleteDuplicate guards the real
// production scenario this fix targets: two releases sharing an exact code
// (e.g. "THPA-15" from two different site/scraper registrations), one fully
// scraped and one an essentially empty placeholder. Since one of them
// strictly has more scraped metadata, this is no longer treated as
// ambiguous - the fully-scraped one is picked automatically.
func TestSelectExactReleaseIDPicksMostCompleteDuplicate(t *testing.T) {
	results := []provider.Metadata{
		{Code: "THPA-15", ReleaseID: 9001},
		{
			Code: "THPA-15", ReleaseID: 9099,
			PremiereDate: "2026-07-10", Studio: "GIGA", Performers: []string{"Honoka Ashina"},
			Genres: []string{"Fighters"}, StashSceneID: "stash-1",
		},
	}
	id, ok := selectExactReleaseID(results, "THPA-15")
	if !ok || id != 9099 {
		t.Fatalf("selectExactReleaseID = (%d, %v), want (9099, true) - the fully-scraped duplicate", id, ok)
	}
}

// TestSelectExactReleaseIDRejectsThreeWayTie guards the tie-break loop
// itself, not just the two-candidate case: a later candidate matching the
// current best score must not silently win by virtue of appearing later in
// the slice.
func TestSelectExactReleaseIDChoosesStableThreeWayTie(t *testing.T) {
	results := []provider.Metadata{
		{Code: "ADN-131", ReleaseID: 9001, Studio: "A"},
		{Code: "ADN-131", ReleaseID: 9002, Studio: "B"},
		{Code: "ADN-131", ReleaseID: 9003, Studio: "C"},
	}
	if id, ok := selectExactReleaseID(results, "ADN-131"); !ok || id != 9001 {
		t.Fatalf("three-way duplicate id=%d ok=%v", id, ok)
	}
}

func TestSelectExactReleaseIDIgnoresPartialOrFuzzyTitles(t *testing.T) {
	// A fuzzy/substring hit is exactly what Silo's own scorer already tried
	// and rejected - this helper must never accept one either.
	results := []provider.Metadata{{Code: "ADN-131", ReleaseID: 9001}}
	if _, ok := selectExactReleaseID(results, "ADN-131 Some Extra Title Text"); ok {
		t.Fatal("expected no match for a non-exact title")
	}
}

func TestSelectExactReleaseIDTrimsWhitespace(t *testing.T) {
	results := []provider.Metadata{{Code: " ADN-131 ", ReleaseID: 9001}}
	id, ok := selectExactReleaseID(results, "ADN-131")
	if !ok || id != 9001 {
		t.Fatalf("selectExactReleaseID = (%d, %v), want (9001, true)", id, ok)
	}
}

func TestSelectExactStashProviderID(t *testing.T) {
	rows := []provider.Metadata{{ProviderID: "stash:11631", Code: "ad-359"}, {ProviderID: "stash:2", Code: "OTHER-1"}}
	id, ok := selectExactStashProviderID(rows, "AD-359")
	if !ok || id != "stash:11631" {
		t.Fatalf("id=%q ok=%v", id, ok)
	}
	rows = append(rows, provider.Metadata{ProviderID: "stash:3", Code: "AD-359"})
	if _, ok := selectExactStashProviderID(rows, "AD-359"); ok {
		t.Fatal("ambiguous Stash scenes must not be forced")
	}
}

func TestScheduledMatchSelectsStashOnlySceneByFilename(t *testing.T) {
	jav := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/integrations/silo/search" || r.URL.Query().Get("q") != "ad-359" {
			t.Fatalf("unexpected JAVBeacon lookup %s", r.URL.String())
		}
		_, _ = w.Write([]byte(`{"items":[{"provider_id":"stash:11631","stash_scene_id":"11631","code":"ad-359","title":"Stash scene"}],"total":1}`))
	}))
	defer jav.Close()
	silo := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/files") {
			t.Fatalf("unexpected Silo lookup %s", r.URL.String())
		}
		_, _ = w.Write([]byte(`{"items":[{"file_path":"/collections/jav/ad-359.avi"}],"page":{"has_more":false}}`))
	}))
	defer silo.Close()
	p := provider.NewProvider()
	p.Configure(provider.Config{BaseURL: jav.URL, APIKey: "test"})
	task := &collectionSyncTaskServer{runtime: &runtimeServer{provider: p}}
	id, ok, err := task.exactProviderIDForItem(t.Context(), provider.NewSiloClient(silo.URL, "test"), provider.UnmatchedItem{ContentID: "local-1", Title: "Wrong parsed title"})
	if err != nil || !ok || id != "stash:11631" {
		t.Fatalf("provider id=%q ok=%v err=%v", id, ok, err)
	}
}

func TestExactCodeMatchesHyphenatedAndCompactForms(t *testing.T) {
	rows := []provider.Metadata{{Code: "PMID008", ReleaseID: 12}}
	for _, q := range []string{"PMID008", "PMID-008", "PMID_008"} {
		if id, ok := selectExactReleaseID(rows, q); !ok || id != 12 {
			t.Fatalf("%s: id=%d ok=%v", q, id, ok)
		}
	}
}

func TestFilenameReleaseCodeWithQualitySuffix(t *testing.T) {
	for input, want := range map[string]string{
		"PMID-008 [WEBDL-2160p]":     "PMID-008",
		"gxxd06 - alternate":         "gxxd-06",
		"Washing Time [WEBDL-2160p]": "",
	} {
		if got := filenameReleaseCode(input); got != want {
			t.Fatalf("%q => %q, want %q", input, got, want)
		}
	}
}

func TestExactProviderForSceneRequiresSameSceneAndCast(t *testing.T) {
	results := []provider.Metadata{
		{ReleaseID: 9, Code: "ZIZG-020", StashSceneID: "999", Performers: []string{"Wrong"}},
		{ReleaseID: 8, Code: "ZIZG-020", StashSceneID: "42814"},
		{ReleaseID: 7, Code: "ZIZG-020", StashSceneID: "42814", Performers: []string{"Right"}},
	}
	if got := exactProviderForScene(results, "zizg020", "42814"); got != "7" {
		t.Fatalf("provider = %q, want 7", got)
	}
	if got := exactProviderForScene(results, "zizg020", "999"); got != "9" {
		t.Fatalf("other scene = %q, want 9", got)
	}
	if got := exactProviderForScene(results, "zizg020", "1000"); got != "" {
		t.Fatalf("unrelated scene = %q", got)
	}
}

func TestExactProviderForSceneAcceptsStashOnly(t *testing.T) {
	results := []provider.Metadata{{ProviderID: "stash:42815", Code: "SHKD-694", StashSceneID: "42815", Performers: []string{"Actor"}}}
	if got := exactProviderForScene(results, "SHKD-694", "42815"); got != "stash:42815" {
		t.Fatalf("provider = %q", got)
	}
}

func TestStashSceneFromArtwork(t *testing.T) {
	if got := stashSceneFromArtwork("https://jav.example/api/v1/integrations/silo/stash/scenes/42814/cover?api_key=secret"); got != "42814" {
		t.Fatalf("scene = %q", got)
	}
	if got := stashSceneFromArtwork("https://jav.example/other/42814"); got != "" {
		t.Fatalf("unexpected scene = %q", got)
	}
}
