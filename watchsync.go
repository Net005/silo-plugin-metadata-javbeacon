package main

import (
	"context"
	"strconv"

	pluginv1 "github.com/Silo-Server/silo-plugin-sdk/pkg/pluginproto/silo/plugin/v1"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/Net005/silo-plugin-metadata-javbeacon/provider"
)

// watchSyncServer implements watch_sync_provider.v1 (id "javbeacon"), letting
// Silo forward playback/scrobble events into JAVBeacon's own playback engine
// (POST /api/v1/integrations/silo/playback) instead of Silo tracking watched
// state independently. Unlike a real third-party service (Trakt, AniList),
// there is nothing to authorize per-user here: JAVBeacon is a single
// self-hosted instance already configured globally via the "connection"
// config entry (see main.go's Configure), so ExchangeAPIKey/RefreshCredentials/
// GetAccount are lightweight formalities that satisfy the WatchSyncProvider
// contract's connection lifecycle without a second, redundant credential
// store. ListRemoteState and the OAuth/device-code RPCs are intentionally
// left unimplemented (via UnimplementedWatchSyncProviderServer): the
// descriptor advertises only API_KEY auth and scrobble_playback, with no
// import_* flags, so the host has no reason to call them.
type watchSyncServer struct {
	pluginv1.UnimplementedWatchSyncProviderServer
	runtime *runtimeServer
}

const watchSyncAccountSubject = "javbeacon"

// ExchangeAPIKey does not treat req.GetApiKey() as a fresh credential to
// store - the plugin already has JAVBeacon's real API key from its global
// "connection" config. This just confirms the shared JAVBeacon connection is
// configured before Silo shows the watch-sync provider as connected, and
// echoes the submitted key back as the opaque credential so
// WatchSyncAuthenticatedContext round-trips something non-empty.
func (s *watchSyncServer) ExchangeAPIKey(_ context.Context, req *pluginv1.WatchSyncExchangeAPIKeyRequest) (*pluginv1.WatchSyncCredentialResponse, error) {
	if !s.runtime.provider.Configured() {
		return &pluginv1.WatchSyncCredentialResponse{
			Fault: &pluginv1.WatchSyncFault{
				Code:        pluginv1.WatchSyncFaultCode_WATCH_SYNC_FAULT_CODE_INVALID_CREDENTIAL,
				SafeMessage: "Configure this plugin's JAVBeacon Connection (base URL + API key) before connecting playback sync.",
			},
		}, nil
	}
	apiKey := req.GetApiKey()
	if apiKey == "" {
		apiKey = "javbeacon"
	}
	return &pluginv1.WatchSyncCredentialResponse{
		Credentials: &pluginv1.WatchSyncCredentials{AccessToken: apiKey, TokenType: "bearer"},
		Account:     &pluginv1.WatchSyncAccount{ExternalSubject: watchSyncAccountSubject, Username: "JAVBeacon", DisplayName: "JAVBeacon"},
	}, nil
}

// RefreshCredentials is a no-op success: the shared JAVBeacon connection's
// own API key never expires from this plugin's point of view (JAVBeacon
// invalidates it directly, at which point every RPC below simply fails).
func (s *watchSyncServer) RefreshCredentials(_ context.Context, req *pluginv1.WatchSyncRefreshCredentialsRequest) (*pluginv1.WatchSyncCredentialResponse, error) {
	return &pluginv1.WatchSyncCredentialResponse{Credentials: req.GetContext().GetCredentials()}, nil
}

func (s *watchSyncServer) GetAccount(context.Context, *pluginv1.WatchSyncGetAccountRequest) (*pluginv1.WatchSyncGetAccountResponse, error) {
	return &pluginv1.WatchSyncGetAccountResponse{
		Account: &pluginv1.WatchSyncAccount{ExternalSubject: watchSyncAccountSubject, Username: "JAVBeacon", DisplayName: "JAVBeacon"},
	}, nil
}

// ApplyEvents forwards each scrobble/watch event to JAVBeacon's playback
// engine, mapping Silo's operation vocabulary onto the start/progress/stop
// event shape the Jellyfin plugin already uses successfully. A per-event
// failure is reported as a RETRY (temporary) or REJECTED (permanent) result
// rather than failing the whole batch, per the ApplyEvents contract.
func (s *watchSyncServer) ApplyEvents(ctx context.Context, req *pluginv1.WatchSyncApplyEventsRequest) (*pluginv1.WatchSyncApplyEventsResponse, error) {
	results := make([]*pluginv1.WatchSyncApplyResult, 0, len(req.GetEvents()))
	for _, event := range req.GetEvents() {
		results = append(results, s.applyOne(ctx, event))
	}
	return &pluginv1.WatchSyncApplyEventsResponse{Results: results}, nil
}

func (s *watchSyncServer) applyOne(ctx context.Context, event *pluginv1.WatchSyncEvent) *pluginv1.WatchSyncApplyResult {
	externalIDs := event.GetMedia().GetExternalIds()
	releaseID := releaseIDFromExternalIDs(externalIDs)
	stashSceneID := externalIDs[stashSceneIDProviderKeyLower]
	if stashSceneID == "" {
		stashSceneID, _ = stashProviderID(externalIDs[capabilityID], nil)
	}
	if releaseID == 0 && stashSceneID == "" {
		if sceneID, ok := stashProviderID(event.GetProviderItemKey(), nil); ok {
			stashSceneID = sceneID
		} else if n, err := strconv.ParseInt(event.GetProviderItemKey(), 10, 64); err == nil && n > 0 {
			releaseID = n
		}
	}
	if releaseID == 0 && stashSceneID == "" && event.GetMedia().GetMediaItemId() != "" {
		client := provider.NewSiloClient(s.runtime.provider.SiloBaseURL(), s.runtime.provider.SiloAPIKey())
		if client.Configured() {
			profileID, err := client.PrimaryProfileID(ctx)
			if err == nil {
				var poster, backdrop string
				poster, backdrop, err = client.ItemArtwork(ctx, profileID, event.GetMedia().GetMediaItemId())
				if err == nil {
					stashSceneID = stashSceneFromArtwork(poster)
					if stashSceneID == "" {
						stashSceneID = stashSceneFromArtwork(backdrop)
					}
				}
			}
			if err != nil {
				return &pluginv1.WatchSyncApplyResult{
					EventId: event.GetEventId(),
					Status:  pluginv1.WatchSyncApplyStatus_WATCH_SYNC_APPLY_STATUS_RETRY,
					Fault:   &pluginv1.WatchSyncFault{Code: pluginv1.WatchSyncFaultCode_WATCH_SYNC_FAULT_CODE_TEMPORARY, SafeMessage: err.Error()},
				}
			}
		}
	}
	if releaseID == 0 && stashSceneID == "" {
		// No JAVBeacon ID or scene-specific artwork on this exact local item.
		// Nothing for us to forward. NO_CHANGE, not REJECTED:
		// this is an expected, harmless mismatch, not a malformed event.
		return &pluginv1.WatchSyncApplyResult{EventId: event.GetEventId(), Status: pluginv1.WatchSyncApplyStatus_WATCH_SYNC_APPLY_STATUS_NO_CHANGE}
	}

	// releaseID==0 with a non-empty stashSceneID is the Stash-only path: a
	// scene JAVBeacon was never able to scrape into a release row. JAVBeacon's
	// playback engine still forwards checkpoints, resume, completion, and
	// play/O-count writes for it, keyed purely by stash_scene_id - see
	// jellyfin.Service.Playback in the JAVBeacon repo.
	pb := provider.PlaybackEvent{
		SessionID:       event.GetPlaybackSessionId(),
		ReleaseID:       releaseID,
		StashSceneID:    stashSceneID,
		PositionSeconds: event.GetPositionSeconds(),
		RuntimeSeconds:  event.GetDurationSeconds(),
	}
	switch event.GetOperation() {
	case pluginv1.WatchSyncOperation_WATCH_SYNC_OPERATION_SCROBBLE_START:
		pb.Event = "start"
	case pluginv1.WatchSyncOperation_WATCH_SYNC_OPERATION_SCROBBLE_PAUSE:
		pb.Event = "progress"
		pb.IsPaused = true
	case pluginv1.WatchSyncOperation_WATCH_SYNC_OPERATION_SCROBBLE_STOP:
		pb.Event = "stop"
		pb.IsPlayed = event.GetCompleted()
	case pluginv1.WatchSyncOperation_WATCH_SYNC_OPERATION_MARK_WATCHED:
		pb.Event = "stop"
		pb.IsPlayed = true
	case pluginv1.WatchSyncOperation_WATCH_SYNC_OPERATION_MARK_UNWATCHED:
		// JAVBeacon has no "unmark played" concept driven by Jellyfin/Silo -
		// play/O counts are owned by StashApp and only ever incremented from
		// playback. Acknowledge without forwarding rather than reject.
		return &pluginv1.WatchSyncApplyResult{EventId: event.GetEventId(), Status: pluginv1.WatchSyncApplyStatus_WATCH_SYNC_APPLY_STATUS_NO_CHANGE}
	default:
		return &pluginv1.WatchSyncApplyResult{EventId: event.GetEventId(), Status: pluginv1.WatchSyncApplyStatus_WATCH_SYNC_APPLY_STATUS_NO_CHANGE}
	}
	if pb.SessionID == "" {
		pb.SessionID = event.GetWatchHistoryId()
	}
	if pb.SessionID == "" {
		pb.SessionID = event.GetEventId()
	}
	if occurredAt := event.GetOccurredAt(); occurredAt != nil && occurredAt.IsValid() {
		pb.OccurredAt = occurredAt.AsTime()
	}

	if _, err := s.runtime.provider.ReportPlayback(ctx, pb); err != nil {
		return &pluginv1.WatchSyncApplyResult{
			EventId: event.GetEventId(),
			Status:  pluginv1.WatchSyncApplyStatus_WATCH_SYNC_APPLY_STATUS_RETRY,
			Fault:   &pluginv1.WatchSyncFault{Code: pluginv1.WatchSyncFaultCode_WATCH_SYNC_FAULT_CODE_TEMPORARY, SafeMessage: err.Error()},
		}
	}
	return &pluginv1.WatchSyncApplyResult{EventId: event.GetEventId(), Status: pluginv1.WatchSyncApplyStatus_WATCH_SYNC_APPLY_STATUS_APPLIED}
}

// ListRemoteState implements the import half of watch_sync_provider.v1:
// JAVBeacon -> Silo. Unlike ApplyEvents (Silo's own local playback pushed
// OUT to JAVBeacon), this is the only path an admin's EXISTING JAVBeacon/
// StashApp watched and watchlist history can reach Silo at all - without it,
// a freshly matched Silo library item has no way to inherit history that
// already exists in JAVBeacon, which is exactly why every counter in Silo's
// sync panel (Watched/Progress/Favorites/Watchlist/Exported) read 0: this
// plugin only ever implemented the export direction.
//
// JAVBeacon's LibrarySync snapshot is always small enough (a self-hosted,
// single-user JAV collection) to return in one page, so this always reports
// complete_snapshot=true with no next_page_token - Silo then treats every
// call as the full authoritative state rather than an incremental delta,
// which is simplest and correct here, and avoids needing real cursor-based
// pagination against an endpoint that doesn't support it.
//
// Only WATCHED and WATCHLIST are populated. JAVBeacon has no "favorites"
// concept distinct from its Watchlist, and LibrarySync carries no resume/
// progress position, so PROGRESS and FAVORITE are left unreported rather
// than filled with fabricated data.
func (s *watchSyncServer) ListRemoteState(ctx context.Context, req *pluginv1.WatchSyncListRemoteStateRequest) (*pluginv1.WatchSyncListRemoteStateResponse, error) {
	if !s.runtime.provider.Configured() {
		return &pluginv1.WatchSyncListRemoteStateResponse{
			Fault: &pluginv1.WatchSyncFault{
				Code:        pluginv1.WatchSyncFaultCode_WATCH_SYNC_FAULT_CODE_INVALID_CREDENTIAL,
				SafeMessage: "Configure this plugin's JAVBeacon Connection (base URL + API key) before connecting playback sync.",
			},
		}, nil
	}
	if req.GetPageToken() != "" {
		// Every call already returns the complete snapshot in one page - a page
		// token from an earlier call can never be valid here.
		return &pluginv1.WatchSyncListRemoteStateResponse{CompleteSnapshot: true}, nil
	}

	snapshot, err := s.runtime.provider.LibrarySync(ctx)
	if err != nil {
		return &pluginv1.WatchSyncListRemoteStateResponse{
			Fault: &pluginv1.WatchSyncFault{Code: pluginv1.WatchSyncFaultCode_WATCH_SYNC_FAULT_CODE_TEMPORARY, SafeMessage: err.Error()},
		}, nil
	}

	kinds := watchSyncStateKindSet(req.GetStateKinds())
	byItem := map[string]*pluginv1.WatchSyncRemoteState{}
	var order []string
	stateFor := func(releaseID int64, stashSceneID string) *pluginv1.WatchSyncRemoteState {
		key := strconv.FormatInt(releaseID, 10)
		if releaseID == 0 {
			key = "stash:" + stashSceneID
		}
		if existing, ok := byItem[key]; ok {
			return existing
		}
		state := &pluginv1.WatchSyncRemoteState{
			ProviderItemKey: key,
			Media:           remoteStateMedia(releaseID, stashSceneID),
		}
		byItem[key] = state
		order = append(order, key)
		return state
	}

	if snapshot != nil {
		// JAVBeacon returns the Stash Watchlist newest first. Preserve its
		// ordering before adding watched-only rows to the complete snapshot.
		if kinds[pluginv1.WatchSyncRemoteStateKind_WATCH_SYNC_REMOTE_STATE_KIND_WATCHLIST] {
			for _, item := range snapshot.Watchlist {
				state := stateFor(item.ReleaseID, item.StashSceneID)
				watchlist := &pluginv1.WatchSyncRemoteListState{}
				if !item.WatchlistedAt.IsZero() {
					watchlist.ListedAt = timestamppb.New(item.WatchlistedAt)
				}
				state.Watchlist = watchlist
			}
		}
		if kinds[pluginv1.WatchSyncRemoteStateKind_WATCH_SYNC_REMOTE_STATE_KIND_WATCHED] {
			for _, item := range snapshot.Watched {
				state := stateFor(item.ReleaseID, item.StashSceneID)
				watched := &pluginv1.WatchSyncRemoteWatchedState{PlayCount: int32(max(item.PlayCount, 1))}
				if !item.WatchedAt.IsZero() {
					watched.LastWatchedAt = timestamppb.New(item.WatchedAt)
				}
				state.Watched = watched
			}
		}
	}

	items := make([]*pluginv1.WatchSyncRemoteState, 0, len(order))
	for _, key := range order {
		items = append(items, byItem[key])
	}
	return &pluginv1.WatchSyncListRemoteStateResponse{Items: items, CompleteSnapshot: true}, nil
}

// watchSyncStateKindSet returns which WatchSyncRemoteStateKind values the
// host asked for - an empty request means every kind this plugin supports
// (WATCHED and WATCHLIST; PROGRESS/FAVORITE are never populated regardless).
func watchSyncStateKindSet(requested []pluginv1.WatchSyncRemoteStateKind) map[pluginv1.WatchSyncRemoteStateKind]bool {
	if len(requested) == 0 {
		return map[pluginv1.WatchSyncRemoteStateKind]bool{
			pluginv1.WatchSyncRemoteStateKind_WATCH_SYNC_REMOTE_STATE_KIND_WATCHED:   true,
			pluginv1.WatchSyncRemoteStateKind_WATCH_SYNC_REMOTE_STATE_KIND_WATCHLIST: true,
		}
	}
	set := make(map[pluginv1.WatchSyncRemoteStateKind]bool, len(requested))
	for _, kind := range requested {
		set[kind] = true
	}
	return set
}

// remoteStateMedia builds the WatchSyncMedia Silo uses to match this remote
// state row back to a library item, using the same "javbeacon"/"stash"
// external id keys ApplyEvents and the metadata provider already round-trip.
func remoteStateMedia(releaseID int64, stashSceneID string) *pluginv1.WatchSyncMedia {
	ids := map[string]string{}
	if releaseID > 0 {
		ids[capabilityID] = strconv.FormatInt(releaseID, 10)
	}
	if stashSceneID != "" {
		ids[stashSceneIDProviderKeyLower] = stashSceneID
	}
	return &pluginv1.WatchSyncMedia{
		MediaType:   pluginv1.WatchSyncMediaType_WATCH_SYNC_MEDIA_TYPE_MOVIE,
		ExternalIds: ids,
	}
}

// releaseIDFromExternalIDs reads this plugin's own "javbeacon" external id
// key back out of a WatchSyncMedia (populated originally from this same
// plugin's ProviderSearchResult/MetadataItem.ProviderIds during library
// scan/match, so the round trip is exact).
func releaseIDFromExternalIDs(ids map[string]string) int64 {
	if ids == nil {
		return 0
	}
	raw, ok := ids[capabilityID]
	if !ok {
		return 0
	}
	n, err := strconv.ParseInt(raw, 10, 64)
	if err != nil {
		return 0
	}
	return n
}
