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
	silo := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v2/admin/items/" + id + "/files":
			_, _ = w.Write([]byte(`{"items":[{"library_id":"16","file_path":"/collections/giga/abgd-01.wmv"}],"page":{"has_more":false}}`))
		case "/api/v2/admin/items/" + id + "/refresh-metadata":
			refreshed++
			w.WriteHeader(refreshStatus)
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
	if err != nil || count != 1 || !next.Equal(checked) || refreshed != 2 || acked != 1 {
		t.Fatalf("next=%v count=%d refreshed=%d acked=%d err=%v", next, count, refreshed, acked, err)
	}
}
