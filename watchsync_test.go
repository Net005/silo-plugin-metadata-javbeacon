package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	pluginv1 "github.com/Silo-Server/silo-plugin-sdk/pkg/pluginproto/silo/plugin/v1"

	"github.com/Net005/silo-plugin-metadata-javbeacon/provider"
)

// TestListRemoteStateImportsWatchedAndWatchlist guards the JAVBeacon -> Silo
// import direction: a live report showed every counter in Silo's watch-sync
// panel (Watched/Progress/Favorites/Watchlist/Exported) reading 0 despite
// "Connected"/"Synced Just now", traced to this plugin only ever having
// implemented the export direction (ApplyEvents) - ListRemoteState was
// unimplemented, and LibrarySync's Watchlist/Watched fields were never even
// decoded. This exercises the real HTTP round trip against JAVBeacon's own
// library-sync JSON shape, not just a mocked provider.
func TestListRemoteStateImportsWatchedAndWatchlist(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/integrations/silo/library-sync" {
			t.Fatalf("unexpected path %q", r.URL.Path)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"revision": "rev-1",
			"watchlist": []map[string]any{
				{"release_id": 9001, "stash_scene_id": "scene-1", "watchlisted_at": "2026-09-01T00:00:00Z"},
			},
			"watched": []map[string]any{
				{"release_id": 9001, "stash_scene_id": "scene-1", "watched_at": "2026-09-20T00:00:00Z"},
				{"release_id": 9002, "stash_scene_id": "", "watched_at": "2026-09-21T00:00:00Z"},
			},
			"filter_presets": []map[string]any{},
		})
	}))
	defer server.Close()

	p := provider.NewProvider()
	p.Configure(provider.Config{BaseURL: server.URL, APIKey: "test-key"})
	ws := &watchSyncServer{runtime: &runtimeServer{provider: p}}

	resp, err := ws.ListRemoteState(t.Context(), &pluginv1.WatchSyncListRemoteStateRequest{})
	if err != nil {
		t.Fatalf("ListRemoteState: %v", err)
	}
	if resp.GetFault() != nil {
		t.Fatalf("unexpected fault: %+v", resp.GetFault())
	}
	if !resp.GetCompleteSnapshot() {
		t.Fatal("expected complete_snapshot=true")
	}
	if resp.GetNextPageToken() != "" {
		t.Fatalf("next_page_token = %q, want empty", resp.GetNextPageToken())
	}
	if len(resp.GetItems()) != 2 {
		t.Fatalf("items = %d, want 2 (one merged row per release)", len(resp.GetItems()))
	}

	var release9001, release9002 *pluginv1.WatchSyncRemoteState
	for _, item := range resp.GetItems() {
		switch item.GetMedia().GetExternalIds()[capabilityID] {
		case "9001":
			release9001 = item
		case "9002":
			release9002 = item
		}
	}
	if release9001 == nil || release9002 == nil {
		t.Fatalf("missing expected releases in items: %+v", resp.GetItems())
	}

	// 9001 is on both Watchlist and Watched - must merge into ONE row with
	// both typed sub-states set, not two separate rows for the same item.
	if release9001.GetWatched() == nil || release9001.GetWatched().GetPlayCount() != 1 {
		t.Fatalf("release 9001 Watched = %+v, want PlayCount=1", release9001.GetWatched())
	}
	if release9001.GetWatchlist() == nil {
		t.Fatal("release 9001 expected a Watchlist state (present on both lists)")
	}
	if release9001.GetMedia().GetExternalIds()["stash"] != "scene-1" {
		t.Fatalf("release 9001 stash external id = %q, want scene-1", release9001.GetMedia().GetExternalIds()["stash"])
	}

	// 9002 is Watched only - must not fabricate a Watchlist state for it.
	if release9002.GetWatched() == nil {
		t.Fatal("release 9002 expected a Watched state")
	}
	if release9002.GetWatchlist() != nil {
		t.Fatalf("release 9002 unexpectedly has a Watchlist state: %+v", release9002.GetWatchlist())
	}
}

// TestListRemoteStateRespectsRequestedStateKinds guards against reporting
// state kinds the host didn't ask for.
func TestListRemoteStateRespectsRequestedStateKinds(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"revision":  "rev-1",
			"watchlist": []map[string]any{{"release_id": 9001, "watchlisted_at": "2026-09-01T00:00:00Z"}},
			"watched":   []map[string]any{{"release_id": 9001, "watched_at": "2026-09-20T00:00:00Z"}},
		})
	}))
	defer server.Close()

	p := provider.NewProvider()
	p.Configure(provider.Config{BaseURL: server.URL, APIKey: "test-key"})
	ws := &watchSyncServer{runtime: &runtimeServer{provider: p}}

	resp, err := ws.ListRemoteState(t.Context(), &pluginv1.WatchSyncListRemoteStateRequest{
		StateKinds: []pluginv1.WatchSyncRemoteStateKind{pluginv1.WatchSyncRemoteStateKind_WATCH_SYNC_REMOTE_STATE_KIND_WATCHED},
	})
	if err != nil {
		t.Fatalf("ListRemoteState: %v", err)
	}
	if len(resp.GetItems()) != 1 {
		t.Fatalf("items = %d, want 1", len(resp.GetItems()))
	}
	if resp.GetItems()[0].GetWatchlist() != nil {
		t.Fatal("did not request WATCHLIST, but got a Watchlist state anyway")
	}
	if resp.GetItems()[0].GetWatched() == nil {
		t.Fatal("expected a Watched state (was requested)")
	}
}

// TestListRemoteStateRequiresConfiguredConnection guards the same
// not-configured fault path ExchangeAPIKey already uses.
func TestListRemoteStateRequiresConfiguredConnection(t *testing.T) {
	ws := &watchSyncServer{runtime: &runtimeServer{provider: provider.NewProvider()}}
	resp, err := ws.ListRemoteState(t.Context(), &pluginv1.WatchSyncListRemoteStateRequest{})
	if err != nil {
		t.Fatalf("ListRemoteState: %v", err)
	}
	if resp.GetFault() == nil || resp.GetFault().GetCode() != pluginv1.WatchSyncFaultCode_WATCH_SYNC_FAULT_CODE_INVALID_CREDENTIAL {
		t.Fatalf("fault = %+v, want INVALID_CREDENTIAL", resp.GetFault())
	}
}
