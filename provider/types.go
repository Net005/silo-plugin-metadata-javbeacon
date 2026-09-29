package provider

import "time"

// Metadata mirrors the JSON shape JAVBeacon's own dedicated Silo integration
// endpoints return (/api/v1/integrations/silo/search and
// /api/v1/integrations/silo/releases/{id}), served by JAVBeacon's
// independent internal/silo.Metadata DTO (not shared with its Jellyfin
// plugin's internal/jellyfin.Metadata since JAVBeacon v1.0.239 - see that
// repo's CHANGELOG). It has no Tags or PerformerIDs field - confirmed unused
// anywhere in this plugin (Genres already carries everything Tags would have,
// and GetPersonDetail below never routes through a performer id) - so
// JAVBeacon's server-side DTO omits them rather than sending dead weight.
//
// CoverPath, CoverBackdropPath, StashScreenshotURL, and every entry in
// BackdropURLs are already complete, JAVBeacon-relative paths (e.g.
// "/covers/123/jellyfin-primary", "/screenshots/123/0") - not bare IDs - so
// resolving them is just base_url + path + the api_key query parameter
// JAVBeacon's own auth middleware accepts as an alternative to the
// Authorization header (see Client.ImageURL). Nothing here needs to
// reconstruct a JAVBeacon URL convention on its own.
type Metadata struct {
	ReleaseID         int64    `json:"release_id"`
	ProviderID        string   `json:"provider_id,omitempty"`
	StashSceneID      string   `json:"stash_scene_id,omitempty"`
	Code              string   `json:"code"`
	Title             string   `json:"title"`
	OriginalTitle     string   `json:"original_title,omitempty"`
	Overview          string   `json:"overview,omitempty"`
	PremiereDate      string   `json:"premiere_date,omitempty"`
	ProductionYear    int      `json:"production_year,omitempty"`
	Studio            string   `json:"studio,omitempty"`
	Label             string   `json:"label,omitempty"`
	Performers        []string `json:"performers,omitempty"`
	Directors         []string `json:"directors,omitempty"`
	Genres            []string `json:"genres,omitempty"`
	RuntimeSeconds    int64    `json:"runtime_seconds,omitempty"`
	CoverPath         string   `json:"cover_path,omitempty"`
	CoverBackdropPath string   `json:"cover_backdrop_path,omitempty"`
	BackdropURLs      []string `json:"backdrop_urls,omitempty"`
	// StashScreenshotURL is a JAVBeacon-proxied StashApp scene screenshot,
	// populated only when the release has no JAVBeacon-scraped cover of its
	// own. Resolved via GetImages as an additional poster/backdrop candidate
	// alongside (never instead of) CoverPath/CoverBackdropPath - see
	// metadataItemFromResult/GetImages in main.go. New in JAVBeacon v1.0.239;
	// this plugin previously had no Stash-screenshot gap-fill at all, unlike
	// the Jellyfin plugin.
	StashScreenshotURL string `json:"stash_screenshot_url,omitempty"`
	StashPosterURL     string `json:"stash_poster_url,omitempty"`
	SourceURL          string `json:"source_url,omitempty"`
	// CollectionNames lists every JAVBeacon saved filter set this release
	// currently belongs to, only populated on the single-release fetch
	// (GetMetadata). Ordered Silo collections are synchronized separately
	// from the library-sync snapshot.
	CollectionNames []string `json:"collection_names,omitempty"`
	// PerformerImages maps a performer's display name (as it appears in
	// Performers) to a JAVBeacon-proxied StashApp portrait URL - see
	// metadataItemFromResult in main.go, which sets PersonRecord.PhotoPath
	// from this. Only populated on the single-release fetch (GetMetadata).
	PerformerImages  map[string]string          `json:"performer_images,omitempty"`
	PerformerDetails map[string]PerformerDetail `json:"performer_details,omitempty"`
	// Watchlist mirrors JAVBeacon's own Watchlist membership for this
	// release, unlike CollectionNames/PerformerImages it costs JAVBeacon
	// nothing extra to populate and is present on every fetch, including
	// search results. The collection sync uses the ordered library snapshot.
	Watchlist   bool              `json:"watchlist"`
	ProviderIDs map[string]string `json:"provider_ids"`
}

// PerformerDetail is StashApp data already included in JAVBeacon's scene query.
type PerformerDetail struct {
	StashID   string `json:"stash_id"`
	Birthdate string `json:"birthdate,omitempty"`
}

// PerformerBio is JAVBeacon's authenticated Stash performer detail response.
type PerformerBio struct {
	ID           string `json:"id"`
	Name         string `json:"name"`
	Birthdate    string `json:"birthdate,omitempty"`
	DeathDate    string `json:"death_date,omitempty"`
	Details      string `json:"details,omitempty"`
	Country      string `json:"country,omitempty"`
	CareerLength string `json:"career_length,omitempty"`
	HeightCM     int    `json:"height_cm,omitempty"`
}

// PlaybackEvent mirrors JAVBeacon's internal/jellyfin.PlaybackEvent JSON shape
// (already provider-agnostic) for POST /api/v1/integrations/silo/playback.
// ReleaseID and StashSceneID follow the same either/or rule as the server
// side: when ReleaseID is 0, JAVBeacon keys and forwards the session purely by
// StashSceneID, with no JAVBeacon release row required at all - see
// jellyfin.Service.Playback and migrateJellyfinPlaybackReleaseNullable in the
// JAVBeacon repo.
type PlaybackEvent struct {
	Event           string  `json:"event"`
	SessionID       string  `json:"session_id"`
	ReleaseID       int64   `json:"release_id,omitempty"`
	StashSceneID    string  `json:"stash_scene_id,omitempty"`
	PositionSeconds float64 `json:"position_seconds"`
	RuntimeSeconds  float64 `json:"runtime_seconds"`
	IsPaused        bool    `json:"is_paused"`
	IsPlayed        bool    `json:"is_played"`
}

// PlaybackResult mirrors JAVBeacon's internal/jellyfin.PlaybackResult.
type PlaybackResult struct {
	SessionID         string  `json:"session_id"`
	Accumulated       float64 `json:"accumulated_seconds"`
	Forwarded         float64 `json:"forwarded_seconds"`
	ResumeTime        float64 `json:"resume_time_seconds"`
	PlayCounted       bool    `json:"play_counted"`
	CheckpointWritten bool    `json:"checkpoint_written"`
}

// searchResponse is the envelope /api/v1/integrations/silo/search returns.
type searchResponse struct {
	Items []Metadata `json:"items"`
	Total int        `json:"total"`
}

// FilterPresetCollection mirrors JAVBeacon's internal/jellyfin.FilterPresetCollection
// JSON shape: one saved filter set resolved to its current membership.
type FilterPresetCollection struct {
	ID         int64   `json:"id"`
	Name       string  `json:"name"`
	ReleaseIDs []int64 `json:"release_ids"`
}

// LibrarySync mirrors JAVBeacon's internal/jellyfin.LibrarySyncSnapshot JSON
// shape. Watchlist/Watched were originally left undecoded here on the
// (mistaken) assumption that they were purely Jellyfin-collection concepts -
// they are in fact the exact watched/watchlist state this plugin's
// watch_sync_provider.v1 ListRemoteState needs to import into Silo (see
// watchsync.go), so they are decoded the same as FilterPresets.
type LibrarySync struct {
	Revision      string                   `json:"revision"`
	Watchlist     []LibrarySyncItem        `json:"watchlist"`
	Watched       []LibrarySyncItem        `json:"watched"`
	FilterPresets []FilterPresetCollection `json:"filter_presets"`
	ReleaseCodes  map[int64]string         `json:"release_codes,omitempty"`
}

// LibrarySyncItem mirrors JAVBeacon's internal/jellyfin.LibrarySyncItem JSON
// shape - one release's Watchlist or Watched membership.
type LibrarySyncItem struct {
	ReleaseID     int64     `json:"release_id"`
	StashSceneID  string    `json:"stash_scene_id"`
	Title         string    `json:"title,omitempty"`
	Path          string    `json:"path,omitempty"`
	WatchlistedAt time.Time `json:"watchlisted_at,omitempty"`
	WatchedAt     time.Time `json:"watched_at,omitempty"`
	PlayCount     int       `json:"play_count,omitempty"`
}

// ReleaseIDProviderKey and StashSceneIDProviderKey are the keys JAVBeacon's
// own DTO uses in Metadata.ProviderIDs - see the ProviderIDRelease /
// ProviderIDStash constants in JAVBeacon's internal/jellyfin package. They
// are capitalized because that Go package predates this plugin; this
// package's own outward-facing provider_ids map uses the lowercase
// "javbeacon"/"stash" keys instead, matching this plugin's capability id and
// the lowercase convention every other Silo metadata plugin (e.g. TMDB's
// "tmdb"/"imdb"/"tvdb") uses.
const (
	releaseIDProviderKey    = "JAVBeacon"
	stashSceneIDProviderKey = "Stash"
)
