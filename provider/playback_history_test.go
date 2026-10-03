package provider

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestCompletedPlaybackHistoryConsumesAllPages(t *testing.T) {
	pages := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		pages++
		if r.Header.Get("Authorization") != "Bearer key" || r.URL.Query().Get("completed") != "true" || r.URL.Query().Get("profile_id") != "profile" {
			t.Fatalf("unexpected request: %s", r.URL)
		}
		if pages == 1 {
			_ = json.NewEncoder(w).Encode(map[string]any{"items": []map[string]any{{"session_id": "one", "media_item_id": "local-1", "media_type": "movie", "completed": true, "ended_at": "2026-10-03T03:11:14Z"}}, "page": map[string]any{"has_more": true, "next_cursor": "next"}})
		} else {
			if r.URL.Query().Get("cursor") != "next" {
				t.Fatal("missing cursor")
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"items": []map[string]any{{"session_id": "two", "media_item_id": "local-2", "media_type": "movie", "completed": true, "ended_at": "2026-10-02T19:41:46Z"}}, "page": map[string]any{"has_more": false}})
		}
	}))
	defer server.Close()
	got, err := NewSiloClient(server.URL, "key").CompletedPlaybackHistory(context.Background(), "profile")
	if err != nil || pages != 2 || len(got) != 2 {
		t.Fatalf("pages=%d items=%d err=%v", pages, len(got), err)
	}
}
