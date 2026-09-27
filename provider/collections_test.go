package provider

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
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
		case r.URL.Path == "/api/v2/admin/collections" && r.Method == http.MethodPost:
			var data struct {
				Title       string `json:"title"`
				Slug        string `json:"slug"`
				LibraryID   string `json:"library_id"`
				Description string `json:"description"`
			}
			_ = json.NewDecoder(r.Body).Decode(&data)
			collection = siloCollection{ID: "c1", Slug: data.Slug, LibraryID: data.LibraryID, Description: data.Description}
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
