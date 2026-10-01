package provider

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
)

func TestSyncCollectionsCreatesAndReconcilesOrderedMembers(t *testing.T) {
	collection := siloCollection{}
	members := map[string]int{}
	calls := []string{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls = append(calls, r.Method+" "+r.URL.Path)
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.URL.Path == "/api/v2/admin/collections" && r.Method == http.MethodGet:
			if r.URL.RawQuery != "" {
				t.Errorf("unexpected collection list query: %s", r.URL.RawQuery)
			}
			items := []siloCollection{}
			if collection.ID != "" {
				items = append(items, collection)
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"items": items, "page": map[string]any{"has_more": false}})
		case r.URL.Path == "/api/v2/admin/collections/order" && r.Method == http.MethodGet:
			w.Header().Set("ETag", `"collection-order-v1"`)
			ids := []string{}
			if collection.ID != "" {
				ids = append(ids, collection.ID)
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"ordered_ids": ids})
		case r.URL.Path == "/api/v2/admin/collections" && r.Method == http.MethodPost:
			var data struct {
				Title       string `json:"title"`
				Slug        string `json:"slug"`
				LibraryID   string `json:"library_id"`
				Description string `json:"description"`
			}
			_ = json.NewDecoder(r.Body).Decode(&data)
			collection = siloCollection{ID: "c1", Title: data.Title, Slug: data.Slug, LibraryID: data.LibraryID, Description: data.Description}
			_ = json.NewEncoder(w).Encode(collection)
		case r.URL.Path == "/api/v2/admin/collections/c1/items" && r.Method == http.MethodGet:
			items := []map[string]any{}
			for id, pos := range members {
				items = append(items, map[string]any{"media_item_id": id, "position": pos})
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"items": items, "page": map[string]any{"has_more": false}})
		case strings.HasPrefix(r.URL.Path, "/api/v2/admin/collections/c1/items/"):
			id := strings.TrimPrefix(r.URL.Path, "/api/v2/admin/collections/c1/items/")
			if id == "order" && r.Method == http.MethodGet {
				w.Header().Set("ETag", `"order-v1"`)
				_ = json.NewEncoder(w).Encode(map[string]any{"ordered_ids": []string{}})
				return
			}
			if id == "order" {
				if r.Header.Get("If-Match") != `"order-v1"` {
					t.Errorf("missing If-Match on order PUT")
				}
				var body struct {
					Ordered []string `json:"ordered_ids"`
				}
				_ = json.NewDecoder(r.Body).Decode(&body)
				for i, member := range body.Ordered {
					members[member] = i
				}
				w.WriteHeader(http.StatusNoContent)
				return
			}
			if r.Method == http.MethodDelete {
				delete(members, id)
			} else {
				var body struct {
					Position int `json:"position"`
				}
				_ = json.NewDecoder(r.Body).Decode(&body)
				members[id] = body.Position
			}
			w.WriteHeader(http.StatusNoContent)
		default:
			t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer server.Close()
	client := NewSiloClient(server.URL, "test")
	specs := []CollectionSpec{{Kind: "watchlist", Name: "Watchlist", LibraryID: "lib", MediaIDs: []string{"a", "b"}}}
	if _, err := client.SyncCollections(context.Background(), specs); err != nil {
		t.Fatal(err)
	}
	if members["a"] != 0 || members["b"] != 1 {
		t.Fatalf("members=%v", members)
	}
	calls = nil
	specs[0].MediaIDs = []string{"b", "c"}
	if _, err := client.SyncCollections(context.Background(), specs); err != nil {
		t.Fatal(err)
	}
	if len(members) != 2 || members["b"] != 0 || members["c"] != 1 {
		t.Fatalf("members=%v", members)
	}
	sawRemove, sawOrder := false, false
	for _, call := range calls {
		if strings.Contains(call, "DELETE ") {
			sawRemove = true
		}
		if strings.Contains(call, "/items/order") {
			sawOrder = true
		}
	}
	if !sawRemove || !sawOrder {
		t.Fatalf("calls=%v", calls)
	}
	specs[0].MediaIDs = []string{"b", "c", "d", "e"}
	changed, complete, err := client.SyncCollectionsBatch(context.Background(), specs, 1)
	if err != nil || complete || changed != 1 {
		t.Fatalf("batch changed=%d complete=%v err=%v", changed, complete, err)
	}
	_, complete, err = client.SyncCollectionsBatch(context.Background(), specs, 0)
	if err != nil || !complete || len(members) != 4 || members["e"] != 3 {
		t.Fatalf("resume complete=%v members=%v err=%v", complete, members, err)
	}
}

func TestAlphabetizeManagedSlotsPreservesOtherCollections(t *testing.T) {
	owned := func(id, title string) siloCollection {
		return siloCollection{ID: id, Title: title, Slug: "javbeacon-preset-" + id, Description: collectionOwner, LibraryID: "lib"}
	}
	byID := map[string]siloCollection{
		"prison":    owned("prison", "Prison"),
		"debt":      owned("debt", "Debt"),
		"watchlist": owned("watchlist", "Watchlist"),
		"user":      {ID: "user", Title: "Mine", LibraryID: "lib"},
	}
	got, changed := alphabetizeManagedSlots([]string{"prison", "user", "watchlist", "debt"}, byID)
	want := []string{"debt", "user", "prison", "watchlist"}
	if !changed || !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v, changed %v; want %v", got, changed, want)
	}
	got, changed = alphabetizeManagedSlots(got, byID)
	if changed || !reflect.DeepEqual(got, want) {
		t.Fatalf("already sorted: got %v, changed %v", got, changed)
	}
}

func TestSyncCollectionsSelectionRenamesAndPrunesOwnedPresets(t *testing.T) {
	kept := siloCollection{ID: "keep", Title: "Prison", Slug: "javbeacon-preset-1-library-lib", LibraryID: "lib", Description: collectionOwner}
	removed := siloCollection{ID: "remove", Title: "Other", Slug: "javbeacon-preset-2-library-lib", LibraryID: "lib", Description: collectionOwner}
	user := siloCollection{ID: "user", Title: "Other", Slug: "my-other", LibraryID: "lib", Description: "User collection"}
	outside := siloCollection{ID: "outside", Title: "Other", Slug: "javbeacon-preset-2-library-other", LibraryID: "other", Description: collectionOwner}
	items := []siloCollection{kept, removed, user, outside}
	deleted := map[string]bool{}
	renamed := false
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.URL.Path == "/api/v2/admin/collections" && r.Method == http.MethodGet:
			live := []siloCollection{}
			for _, item := range items {
				if !deleted[item.ID] {
					live = append(live, item)
				}
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"items": live})
		case r.URL.Path == "/api/v2/admin/collections/order" && r.Method == http.MethodGet:
			w.Header().Set("ETag", `"order"`)
			_ = json.NewEncoder(w).Encode(map[string]any{"ordered_ids": []string{"keep", "remove", "user", "outside"}})
		case r.URL.Path == "/api/v2/admin/collections/order" && r.Method == http.MethodPut:
			w.WriteHeader(http.StatusNoContent)
		case r.URL.Path == "/api/v2/admin/collections/remove" && r.Method == http.MethodDelete:
			deleted["remove"] = true
			w.WriteHeader(http.StatusNoContent)
		case r.URL.Path == "/api/v2/admin/collections/keep" && r.Method == http.MethodGet:
			w.Header().Set("ETag", `"item"`)
			_ = json.NewEncoder(w).Encode(kept)
		case r.URL.Path == "/api/v2/admin/collections/keep" && r.Method == http.MethodPatch:
			if r.Header.Get("If-Match") != `"item"` {
				t.Error("missing title ETag")
			}
			var body map[string]string
			_ = json.NewDecoder(r.Body).Decode(&body)
			renamed = body["title"] == "Stash | Prison"
			w.WriteHeader(http.StatusNoContent)
		case r.URL.Path == "/api/v2/admin/collections/keep/items" && r.Method == http.MethodGet:
			_ = json.NewEncoder(w).Encode(map[string]any{"items": []any{}, "page": map[string]any{"has_more": false}})
		case r.URL.Path == "/api/v2/admin/collections/keep" && r.Method == http.MethodDelete:
			t.Error("deleted selected collection")
		default:
			t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer server.Close()
	client := NewSiloClient(server.URL, "key")
	specs := []CollectionSpec{{Kind: "preset", PresetID: 1, Name: "Stash | Prison", LibraryID: "lib"}}
	_, _, err := client.SyncCollectionsBatch(context.Background(), specs, 0, true)
	if err != nil {
		t.Fatal(err)
	}
	if !deleted["remove"] || deleted["user"] || deleted["outside"] || !renamed {
		t.Fatalf("deleted=%v renamed=%v", deleted, renamed)
	}
}
