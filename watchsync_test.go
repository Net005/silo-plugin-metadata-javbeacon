package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	pluginv1 "github.com/Silo-Server/silo-plugin-sdk/pkg/pluginproto/silo/plugin/v1"
	"google.golang.org/protobuf/types/known/timestamppb"

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

func TestListRemoteStateKeepsStashWatchlistOrderAndDistinctScenes(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"revision": "stash-rev",
			"watchlist": []map[string]any{
				{"release_id": 0, "stash_scene_id": "new-scene", "watchlisted_at": "2026-09-27T10:00:00Z"},
				{"release_id": 9, "stash_scene_id": "linked-scene", "watchlisted_at": "2026-09-26T10:00:00Z"},
				{"release_id": 0, "stash_scene_id": "old-scene", "watchlisted_at": "2026-09-25T10:00:00Z"},
			},
			"watched": []map[string]any{{"release_id": 9, "stash_scene_id": "linked-scene"}},
		})
	}))
	defer server.Close()
	p := provider.NewProvider()
	p.Configure(provider.Config{BaseURL: server.URL, APIKey: "test-key"})
	ws := &watchSyncServer{runtime: &runtimeServer{provider: p}}
	resp, err := ws.ListRemoteState(t.Context(), &pluginv1.WatchSyncListRemoteStateRequest{})
	if err != nil || len(resp.GetItems()) != 3 {
		t.Fatalf("items=%+v err=%v", resp.GetItems(), err)
	}
	for i, want := range []string{"stash:new-scene", "9", "stash:old-scene"} {
		if got := resp.GetItems()[i].GetProviderItemKey(); got != want {
			t.Fatalf("item %d key=%q, want %q", i, got, want)
		}
	}
	if _, ok := resp.GetItems()[0].GetMedia().GetExternalIds()[capabilityID]; ok {
		t.Fatal("Stash-only scene must not have a fabricated JAVBeacon release ID")
	}
}

func TestApplyWatchedEventUsesProviderKeyAndStableSession(t *testing.T) {
	occurredAt := time.Date(2026, 10, 2, 19, 41, 46, 0, time.UTC)
	var got provider.PlaybackEvent
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/integrations/silo/playback" {
			t.Fatalf("unexpected path %q", r.URL.Path)
		}
		if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
			t.Fatalf("decode playback: %v", err)
		}
		_ = json.NewEncoder(w).Encode(provider.PlaybackResult{PlayCounted: true})
	}))
	defer server.Close()
	p := provider.NewProvider()
	p.Configure(provider.Config{BaseURL: server.URL, APIKey: "test-key"})
	ws := &watchSyncServer{runtime: &runtimeServer{provider: p}}
	event := &pluginv1.WatchSyncEvent{
		EventId: "watched-1", Operation: pluginv1.WatchSyncOperation_WATCH_SYNC_OPERATION_MARK_WATCHED,
		Media: &pluginv1.WatchSyncMedia{MediaItemId: "local-item"}, ProviderItemKey: "stash:41307",
		OccurredAt: timestamppb.New(occurredAt),
	}
	result := ws.applyOne(t.Context(), event)
	if result.GetStatus() != pluginv1.WatchSyncApplyStatus_WATCH_SYNC_APPLY_STATUS_APPLIED {
		t.Fatalf("status = %v, fault = %+v", result.GetStatus(), result.GetFault())
	}
	if got.StashSceneID != "41307" || got.ReleaseID != 0 || got.SessionID != "watched-1" || got.Event != "stop" || !got.IsPlayed || !got.OccurredAt.Equal(occurredAt) {
		t.Fatalf("forwarded playback = %+v", got)
	}
}

func TestApplyWatchedEventAcceptsStashOnlyProviderID(t *testing.T) {
	var got provider.PlaybackEvent
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
			t.Fatalf("decode playback: %v", err)
		}
		_ = json.NewEncoder(w).Encode(provider.PlaybackResult{PlayCounted: true})
	}))
	defer server.Close()
	p := provider.NewProvider()
	p.Configure(provider.Config{BaseURL: server.URL, APIKey: "test-key"})
	ws := &watchSyncServer{runtime: &runtimeServer{provider: p}}
	event := &pluginv1.WatchSyncEvent{
		EventId: "watched-2", Operation: pluginv1.WatchSyncOperation_WATCH_SYNC_OPERATION_MARK_WATCHED,
		Media: &pluginv1.WatchSyncMedia{ExternalIds: map[string]string{"javbeacon": "stash:41307"}},
	}
	result := ws.applyOne(t.Context(), event)
	if result.GetStatus() != pluginv1.WatchSyncApplyStatus_WATCH_SYNC_APPLY_STATUS_APPLIED || got.StashSceneID != "41307" {
		t.Fatalf("status=%v playback=%+v", result.GetStatus(), got)
	}
}

// Silo's watch adapter sends a local media ID but filters custom provider IDs.
// Resolve the exact item artwork before forwarding a completed play.
func TestApplyWatchedEventResolvesLocalStashArtwork(t *testing.T) {
	var got provider.PlaybackEvent
	jav := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/integrations/silo/playback" {
			t.Fatalf("unexpected JAVBeacon path %q", r.URL.Path)
		}
		if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
			t.Fatal(err)
		}
		_ = json.NewEncoder(w).Encode(provider.PlaybackResult{PlayCounted: true})
	}))
	defer jav.Close()
	silo := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v2/profiles":
			_ = json.NewEncoder(w).Encode(map[string]any{"items": []map[string]string{{"id": "profile-1"}}})
		case "/api/v2/catalog/items/local-c93b10a21c5cfb4c67be6ebdf0b6":
			if r.Header.Get("X-Profile-Id") != "profile-1" {
				t.Fatalf("missing profile header")
			}
			_ = json.NewEncoder(w).Encode(map[string]any{
				"content_id": "local-c93b10a21c5cfb4c67be6ebdf0b6",
				"poster_url": "https://jav.example/api/v1/integrations/silo/stash/scenes/42936/poster?token=redacted",
			})
		default:
			t.Fatalf("unexpected Silo path %q", r.URL.Path)
		}
	}))
	defer silo.Close()
	p := provider.NewProvider()
	p.Configure(provider.Config{BaseURL: jav.URL, APIKey: "test-key"})
	p.ConfigureSiloConnection(silo.URL, "", "silo-key")
	ws := &watchSyncServer{runtime: &runtimeServer{provider: p}}
	event := &pluginv1.WatchSyncEvent{
		EventId: "history-1", Operation: pluginv1.WatchSyncOperation_WATCH_SYNC_OPERATION_MARK_WATCHED,
		Media: &pluginv1.WatchSyncMedia{MediaItemId: "local-c93b10a21c5cfb4c67be6ebdf0b6"},
	}
	result := ws.applyOne(t.Context(), event)
	if result.GetStatus() != pluginv1.WatchSyncApplyStatus_WATCH_SYNC_APPLY_STATUS_APPLIED {
		t.Fatalf("status=%v fault=%+v", result.GetStatus(), result.GetFault())
	}
	if got.StashSceneID != "42936" || got.ReleaseID != 0 || got.SessionID != "history-1" || !got.IsPlayed {
		t.Fatalf("forwarded playback = %+v", got)
	}
}
