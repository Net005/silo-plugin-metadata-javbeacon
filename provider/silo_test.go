package provider

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

// TestRefreshItemMetadataUsesCompleteMode guards against regressing back to
// "quick" - a live report of collection genre tags never appearing after
// this task ran is consistent with "quick" not re-applying GetMetadata's
// genre list (Silo's own OpenAPI spec documents no difference between the
// two modes, so this was never actually verified when first written).
func TestRefreshItemMetadataUsesCompleteMode(t *testing.T) {
	var gotBody map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		w.WriteHeader(http.StatusAccepted)
	}))
	defer server.Close()

	client := NewSiloClient(server.URL, "test-key")
	if err := client.RefreshItemMetadata(t.Context(), "9001"); err != nil {
		t.Fatalf("RefreshItemMetadata: %v", err)
	}
	if gotBody["mode"] != "complete" {
		t.Fatalf("mode = %v, want \"complete\"", gotBody["mode"])
	}
}

func TestListUnmatchedItemsParsesPageAndCursor(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v2/libraries/unmatched-items" {
			t.Fatalf("unexpected path %q", r.URL.Path)
		}
		if r.Header.Get("Authorization") != "Bearer test-key" {
			t.Fatalf("missing/incorrect Authorization header: %q", r.Header.Get("Authorization"))
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"total": 1,
			"items": []map[string]any{
				{
					"content_id":   "movie:adn-131",
					"content_type": "movie",
					"library_id":   "1",
					"library_name": "Movies",
					"status":       "ambiguous",
					"title":        "ADN-131",
					"year":         0,
				},
			},
			"page": map[string]any{"has_more": true, "next_cursor": "eyJvIjo1MH0"},
		})
	}))
	defer server.Close()

	client := NewSiloClient(server.URL, "test-key")
	items, next, err := client.ListUnmatchedItems(t.Context(), "")
	if err != nil {
		t.Fatalf("ListUnmatchedItems: %v", err)
	}
	if len(items) != 1 || items[0].ContentID != "movie:adn-131" || items[0].Title != "ADN-131" {
		t.Fatalf("unexpected items: %+v", items)
	}
	if next != "eyJvIjo1MH0" {
		t.Fatalf("next cursor = %q, want the page's next_cursor", next)
	}
}

func TestListUnmatchedItemsNoNextCursorWhenLastPage(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"total": 0,
			"items": []map[string]any{},
			"page":  map[string]any{"has_more": false},
		})
	}))
	defer server.Close()

	client := NewSiloClient(server.URL, "test-key")
	items, next, err := client.ListUnmatchedItems(t.Context(), "")
	if err != nil {
		t.Fatalf("ListUnmatchedItems: %v", err)
	}
	if len(items) != 0 || next != "" {
		t.Fatalf("expected an empty last page, got items=%+v next=%q", items, next)
	}
}

func TestApplyMatchSendsProviderIDsAndLibraryID(t *testing.T) {
	var gotPath string
	var gotBody map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	client := NewSiloClient(server.URL, "test-key")
	if err := client.ApplyMatch(t.Context(), "movie:adn-131", "1", "9001"); err != nil {
		t.Fatalf("ApplyMatch: %v", err)
	}
	if gotPath != "/api/v2/admin/items/movie:adn-131/match/apply" {
		t.Fatalf("path = %q", gotPath)
	}
	providerIDs, _ := gotBody["provider_ids"].(map[string]any)
	if providerIDs["javbeacon"] != "9001" {
		t.Fatalf("provider_ids = %+v, want javbeacon=9001", providerIDs)
	}
	if gotBody["library_id"] != "1" {
		t.Fatalf("library_id = %+v, want 1", gotBody["library_id"])
	}
}

func TestApplyMatchReturnsErrorOnNon2xx(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `{"detail":"no such item"}`, http.StatusNotFound)
	}))
	defer server.Close()

	client := NewSiloClient(server.URL, "test-key")
	err := client.ApplyMatch(t.Context(), "movie:missing", "1", "9001")
	if err == nil {
		t.Fatal("expected an error for a 404 response")
	}
}

func TestItemFilePathsPagesAllFilenames(t *testing.T) {
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if r.Header.Get("Authorization") != "Bearer secret" {
			t.Error("missing Silo API key")
		}
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Query().Get("cursor") == "" {
			_, _ = w.Write([]byte(`{"items":[{"file_path":"/collections/jav/AD-359.avi"}],"page":{"has_more":true,"next_cursor":"next"}}`))
			return
		}
		_, _ = w.Write([]byte(`{"items":[{"file_path":"/collections/jav/AD-359-part2.avi"}],"page":{"has_more":false}}`))
	}))
	defer server.Close()
	paths, err := NewSiloClient(server.URL, "secret").ItemFilePaths(context.Background(), "local-1")
	if err != nil || calls != 2 || len(paths) != 2 {
		t.Fatalf("paths=%v calls=%d err=%v", paths, calls, err)
	}
}
