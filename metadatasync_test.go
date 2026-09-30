package main

import (
	"context"
	"encoding/json"
	"github.com/Net005/silo-plugin-metadata-javbeacon/provider"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestResolveChangedItemsUsesLocalUniqueIdentity(t *testing.T) {
	catalog := []provider.CatalogItem{
		{ContentID: "release", Title: "ABC-123", Type: "movie"},
		{ContentID: "stash", Title: "Washing Time", Type: "movie"},
		{ContentID: "duplicate-a", Title: "Same", Type: "movie"},
		{ContentID: "duplicate-b", Title: "Same", Type: "movie"},
	}
	changes := []provider.MetadataChange{
		{ReleaseID: 12, Code: "abc123"},
		{StashSceneID: "27456", Code: "not-a-code", Title: "Washing Time", Path: "/media/Futanari - Washing Time.mp4"},
		{StashSceneID: "x", Title: "Same"},
		{ReleaseID: 12, Code: "ABC-123"},
	}
	ids := resolveChangedItems(changes, catalog)
	if len(ids) != 2 || ids[0].ID != "release" || ids[1].ID != "stash" {
		t.Fatalf("ids=%v", ids)
	}
}

func TestResolveChangedItemsUsesFilenameBeforeChangedTitle(t *testing.T) {
	catalog := []provider.CatalogItem{{ContentID: "scene", Title: "My Original Filename", Type: "movie"}}
	changes := []provider.MetadataChange{{StashSceneID: "1", Title: "New Stash Title", Path: "/media/My Original Filename.mp4"}}
	ids := resolveChangedItems(changes, catalog)
	if len(ids) != 1 || ids[0].ID != "scene" {
		t.Fatalf("ids=%v", ids)
	}
}

func TestSiloLocalContentIDMatchesIndexedFile(t *testing.T) {
	// Confirmed against Silo's live file record for this path.
	if got := siloLocalContentID("/collections/giga/abgd-01.wmv"); got != "local-47f0d25aa796e87c26b665ce2e57" {
		t.Fatalf("content ID=%s", got)
	}
}

func TestChangedMetadataRefreshAcknowledgesOnlyAfterSiloAccepts(t *testing.T) {
	path := "/collections/giga/abgd-01.wmv"
	id := siloLocalContentID(path)
	checked := time.Now().UTC().Add(-time.Second).Truncate(time.Second)
	acked := 0
	jav := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v1/integrations/silo/metadata-changes":
			_ = json.NewEncoder(w).Encode(provider.MetadataChanges{CheckedAt: checked, Items: []provider.MetadataChange{{StashSceneID: "200", Path: path}}})
		case "/api/v1/integrations/silo/metadata-changes/ack":
			acked++
			w.WriteHeader(http.StatusOK)
		default:
			t.Errorf("unexpected JAV request %s", r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer jav.Close()
	refreshStatus := http.StatusAccepted
	refreshed := 0
	jobState := "queued"
	silo := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v2/libraries":
			_, _ = w.Write([]byte(`{"items":[{"id":"16","type":"movies","paths":["/collections/giga"]}]}`))
		case "/api/v2/admin/items/" + id + "/files":
			_, _ = w.Write([]byte(`{"items":[{"library_id":"16","file_path":"/collections/giga/abgd-01.wmv"}],"page":{"has_more":false}}`))
		case "/api/v2/admin/items/" + id + "/refresh-metadata":
			refreshed++
			w.WriteHeader(refreshStatus)
			if refreshStatus == http.StatusAccepted {
				_, _ = w.Write([]byte(`{"id":"job-1"}`))
			}
		case "/api/v2/admin/jobs/job-1":
			_, _ = w.Write([]byte(`{"state":"` + jobState + `"}`))
		default:
			t.Errorf("unexpected Silo request %s", r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer silo.Close()
	p := provider.NewProvider()
	p.Configure(provider.Config{BaseURL: jav.URL, APIKey: "jav-key"})
	p.ConfigureSiloConnection(silo.URL, "16", "silo-key")
	task := &collectionSyncTaskServer{runtime: &runtimeServer{provider: p}}
	refreshStatus = http.StatusTooManyRequests
	if _, _, err := task.syncChangedMetadata(context.Background(), time.Time{}); err == nil || acked != 0 {
		t.Fatalf("failed refresh acknowledged: err=%v acked=%d", err, acked)
	}
	refreshStatus = http.StatusAccepted
	next, count, err := task.syncChangedMetadata(context.Background(), time.Time{})
	if err != nil || !next.IsZero() || count != 1 || refreshed != 2 || acked != 0 {
		t.Fatalf("queued next=%v count=%d refreshed=%d acked=%d err=%v", next, count, refreshed, acked, err)
	}
	next, count, err = task.syncChangedMetadata(context.Background(), time.Time{})
	if err != nil || !next.IsZero() || count != 0 || acked != 0 {
		t.Fatalf("running next=%v count=%d acked=%d err=%v", next, count, acked, err)
	}
	jobState = "failed"
	next, _, err = task.syncChangedMetadata(context.Background(), time.Time{})
	if err != nil || !next.IsZero() || acked != 0 {
		t.Fatalf("failed job was acknowledged: next=%v acked=%d err=%v", next, acked, err)
	}
	task.metadataPending.items[0].retryAt = time.Time{}
	jobState = "queued"
	_, count, err = task.syncChangedMetadata(context.Background(), time.Time{})
	if err != nil || count != 1 || refreshed != 3 {
		t.Fatalf("retry count=%d refreshed=%d err=%v", count, refreshed, err)
	}
	jobState = "succeeded"
	next, _, err = task.syncChangedMetadata(context.Background(), time.Time{})
	if err != nil || !next.Equal(checked) || acked != 1 {
		t.Fatalf("completed next=%v acked=%d err=%v", next, acked, err)
	}

}

func TestLibraryForMetadataPathSelectsLongestLocalRoot(t *testing.T) {
	libraries := []provider.MovieLibrary{{ID: "16", Paths: []string{"/collections"}}, {ID: "18", Paths: []string{"/collections/hentaied/Hentaied"}}, {ID: "19", Paths: []string{"/collections/misc/other"}}}
	if id := libraryForMetadataPath("/collections/hentaied/Hentaied/scene.mp4", libraries); id != "18" {
		t.Fatalf("library=%s", id)
	}
	if id := libraryForMetadataPath("/collections/misc/other/scene.mp4", libraries); id != "19" {
		t.Fatalf("library=%s", id)
	}
	if id := libraryForMetadataPath("/outside/scene.mp4", libraries); id != "" {
		t.Fatalf("outside library=%s", id)
	}
}

func TestChangedStashSceneRefreshesItsOwnSiloLibrary(t *testing.T) {
	path := "/collections/hentaied/Hentaied/scene.mp4"
	id := siloLocalContentID(path)
	checked := time.Now().UTC().Add(-time.Second).Truncate(time.Second)
	acked := false
	jav := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v1/integrations/silo/metadata-changes" {
			_ = json.NewEncoder(w).Encode(provider.MetadataChanges{CheckedAt: checked, Items: []provider.MetadataChange{{StashSceneID: "1056", Path: path}}})
		} else if r.URL.Path == "/api/v1/integrations/silo/metadata-changes/ack" {
			acked = true
			w.WriteHeader(http.StatusNoContent)
		} else {
			t.Errorf("JAV path=%s", r.URL.Path)
		}
	}))
	defer jav.Close()
	silo := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v2/libraries":
			_, _ = w.Write([]byte(`{"items":[{"id":"16","type":"movies","paths":["/collections/giga"]},{"id":"18","type":"movies","paths":["/collections/hentaied/Hentaied"]}]}`))
		case "/api/v2/admin/items/" + id + "/files":
			_, _ = w.Write([]byte(`{"items":[{"library_id":"18","file_path":"` + path + `"}],"page":{"has_more":false}}`))
		case "/api/v2/admin/items/" + id + "/refresh-metadata":
			w.WriteHeader(http.StatusAccepted)
			_, _ = w.Write([]byte(`{"id":"job-18"}`))
		case "/api/v2/admin/jobs/job-18":
			_, _ = w.Write([]byte(`{"state":"succeeded"}`))
		default:
			t.Errorf("Silo path=%s", r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer silo.Close()
	p := provider.NewProvider()
	p.Configure(provider.Config{BaseURL: jav.URL, APIKey: "key"})
	p.ConfigureSiloConnection(silo.URL, "16", "key")
	task := &collectionSyncTaskServer{runtime: &runtimeServer{provider: p}}
	if _, n, err := task.syncChangedMetadata(context.Background(), time.Time{}); err != nil || n != 1 || acked {
		t.Fatalf("queued n=%d acked=%v err=%v", n, acked, err)
	}
	if next, _, err := task.syncChangedMetadata(context.Background(), time.Time{}); err != nil || !next.Equal(checked) || !acked {
		t.Fatalf("next=%v acked=%v err=%v", next, acked, err)
	}
}
