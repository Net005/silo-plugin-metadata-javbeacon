# JAVBeacon Metadata Plugin for Silo

A [Silo](https://github.com/Silo-Server/silo-server) metadata plugin backed by a
self-hosted [JAVBeacon](https://github.com/Net005/JAVBeacon) instance. It provides
title, actresses/directors, studio, label, tags, release date, overview, and
cover/screenshot artwork for releases JAVBeacon already tracks, and resolves
`javbeacon://` artwork references.

Each JAVBeacon release is served to Silo as a single `movie` item, matched by
JAVBeacon's own release ID - there is no title-search matching step once a
library item is linked to a JAVBeacon release.

## Feature parity with the Jellyfin plugin

This plugin mirrors as much of `integrations/jellyfin/JAVBeacon.Jellyfin` as
Silo's plugin SDK (v0.15.0) actually allows:

- **Playback/O-count reporting** - a `watch_sync_provider.v1` capability
  (`javbeacon`) forwards every scrobble/watch event Silo reports into
  JAVBeacon's own playback engine (checkpointing, resume, completion
  thresholds, StashApp play-count/O-count writeback), the same engine the
  Jellyfin plugin's playback callback uses. Even a scene JAVBeacon never
  scraped into a release row at all can still report playback, forwarded
  purely by `stash_scene_id` - see `internal/jellyfin.Service.Playback` and
  `migrateJellyfinPlaybackReleaseNullable` in the JAVBeacon repo. In practice
  this only fires once something upstream of this plugin (Silo's own
  matching, or a future scan-source capability) attaches a `stash` external
  id to a library item that has no `javbeacon` one. Stash-only scene matches
  use this path when no local JAVBeacon release exists.
- **Watched state in Silo** - the resident collection worker compares StashApp's watched snapshot with local Silo catalog items and marks only unplayed matches watched for the primary profile. Silo's generic watch-provider importer cannot match JAVBeacon/Stash IDs (it only accepts TMDB/IMDb/TVDB), so import is no longer advertised there. Existing playback events continue to flow from Silo to JAVBeacon/StashApp through the watch provider. Historical play counts and timestamps cannot be imported through Silo's watched API.
- **Silo collections** - the StashApp Watchlist and each JAVBeacon saved filter set become real, ordered Silo collections in each matched library. Watchlist follows the StashApp scene update order (newest first); saved filter sets follow JAVBeacon's resolved order. Only items present in the local Silo library are included. Collection artwork rotates every six hours from those members, usually favoring recent releases while sometimes showing older ones. A one-minute background poll applies changes, and the `collection-sync` scheduled task provides a manual and scheduled reconciliation path. The plugin owns only collections bearing its stable slug and description marker.
- **Stash metadata/image gap-fill** - as of JAVBeacon v1.0.239, served by
  JAVBeacon's own dedicated `internal/silo.Service` (previously this reused
  `internal/jellyfin.Service` directly; see "Independent from the Jellyfin
  plugin" below) via the `/api/v1/integrations/silo/*` routes below, so a
  release JAVBeacon only partially scraped still arrives here with StashApp's
  title/studio/performers/genres filled in - no separate work needed on this
  side. The one gap-fill field that plugin *didn't* carry over verbatim is the
  screenshot: this plugin now gets its own dedicated
  `GET .../releases/{id}/stash-cover` fallback image (see "Artwork" below)
  rather than reusing Jellyfin's.
- **Performer birth dates and homepages** - JAVBeacon includes the StashApp
  performer's ID and birthdate in the scene metadata it already fetches.
  After Silo stores a cast member, a bounded background worker finds that
  exact person in Silo and patches the birth date when StashApp has one, plus
  a public JAVBeacon redirect URL as the homepage. This uses Silo's admin
  person API because SDK v0.15.0's `PersonRecord` has no birth-date or
  homepage fields. It requires the configured Silo API key; failures appear
  in the plugin log and are retried on a later metadata fetch.
- **Performer photos** - JAVBeacon never scrapes performer photos itself, so
  `GetMetadata` attaches a `PersonRecord.photo_path` (resolved through the
  same `javbeacon://` scheme as posters/backdrops) for every performer
  StashApp has a photo for, looked up by name against the release's linked
  Stash scene.
- **Collection synchronization** - Configure the Silo API key under **Silo Collection Sync**. The plugin uses Silo's v2 admin collection API to create collections, add and remove members, and preserve source order. It also clears members when a saved filter set is removed. Metadata genres no longer include generated Watchlist or collection tags.
- **Auto-matching items Silo's own scorer rejects** - confirmed live: Silo's
  scan-time matcher scores every candidate against the local file's title
  *and year*, and JAV releases routinely have no production year anywhere in
  the filename. That alone can drag even an exact release-code title match
  below Silo's auto-accept threshold (a real example: an exact `ADN-131` hit
  scored 62/100 and was left sitting as "ambiguous" rather than applied). The
  plugin SDK's `metadata_provider.v1` capability has no way to influence or
  bypass that scoring from `Search`/`GetMetadata`, but Silo's admin REST API
  has a separate, score-free path. The `scheduled_task.v1` **"Auto-match
  unmatched JAVBeacon items"** task (id `match-unmatched`) walks
  `GET /api/v2/libraries/unmatched-items`, checks each actual media filename,
  and accepts only an unambiguous exact JAV release code or Stash scene
  filename match. A Stash-only scene uses the stable `stash:<scene-id>`
  provider ID and its Stash scene ID in the match/apply request. Conflicting
  matches across files remain unmatched. The task applies confident matches
  through `POST /api/v2/admin/items/{id}/match/apply`, bypassing Silo's score
  threshold. It shares the **Silo API key** setting with `collection-sync`.
  JAVBeacon uses fast exact release-code lookups and checks Stash's scene
  index when no local release matches the filename.

- **Playback events for a Stash-only scene** - wired for parity but currently
  unreachable in practice: see the playback bullet above.
- **Not portable** - the Jellyfin Web `+1 O` activity panel is a JavaScript
  injection into Jellyfin Web specifically (via Jellyfin-JavaScript-Injector,
  or the standalone `web/javbeacon-activity.user.js` Tampermonkey build) and
  the Jellyfin collection cover-image picker only exists because Jellyfin's
  own `BoxSet`/`IProviderManager` APIs let a plugin set one; Silo offers no
  equivalent UI-injection point or (per the collections point above)
  collection object to attach a cover to. A Silo-side O-count panel is a
  separate, not-yet-started piece of work (it would need its own DOM
  inspection of Silo's web app and a way to authenticate the browser's calls
  back to JAVBeacon).

## Scan performance

JAVBeacon checks the indexed exact release code before its fuzzy search when
Silo scans a filename that contains a code. The per-release metadata response
uses the last complete collection index while a rebuild runs in the
background, so a slow filter-preset calculation no longer stalls every scan
item. The plugin caches each release response for five minutes and coalesces
concurrent requests, avoiding duplicate JAVBeacon and StashApp calls when
Silo asks for metadata and artwork separately.

Scheduled tasks return promptly and continue as bounded background jobs, since
Silo's task RPC has a short deadline. A second invocation of the same task
reports `already_running`; completion and failures are logged by the plugin.

## Requirements

This plugin talks to five endpoints that must exist on the JAVBeacon
instance it points at:

- `GET /api/v1/integrations/silo/search`
- `GET /api/v1/integrations/silo/releases/{id}`
- `POST /api/v1/integrations/silo/playback`
- `GET /api/v1/integrations/silo/library-sync` (used only by the
  `collection-sync` scheduled task)
- `GET /api/v1/integrations/silo/releases/{id}/stash-cover` (new in
  JAVBeacon v1.0.239; requires a JAVBeacon build at that version or later,
  older builds 404 on this one route only)

All five were added to JAVBeacon alongside this plugin (see
`internal/web/silo.go` in the JAVBeacon repo). As of JAVBeacon v1.0.239 the
first four are served by JAVBeacon's own dedicated `internal/silo.Service` -
independent of the Jellyfin plugin's `internal/jellyfin.Service` - rather
than reusing Jellyfin's integration logic; see "Independent from the
Jellyfin plugin" below. A JAVBeacon build that predates the original four
routes' addition will return 404 for all of them.

### Independent from the Jellyfin plugin

Earlier releases of this plugin (through manifest version 0.3.x) talked to
routes backed by the exact same Go service JAVBeacon's Jellyfin plugin uses.
That sharing turned out to be the root cause of several bugs that only
affected one of the two integrations at a time - a Jellyfin-only collection
sync crash, a scan-path performer-image gap, a `Search` enrichment cost that
only actually hurt this plugin's own automated matching. As of JAVBeacon
v1.0.239, this plugin's four original routes are served by JAVBeacon's own
`internal/silo.Service`, entirely separate from the Jellyfin plugin's
backend, and this plugin now gets its own dedicated Stash-screenshot
fallback endpoint instead of reusing Jellyfin's. The JSON shape for every
field this plugin already reads is unchanged; two Jellyfin-only fields this
plugin never read (`tags`, `performer_ids`) simply stopped being sent.

The `collection-sync` and `match-unmatched` scheduled tasks additionally call
three of Silo's own admin REST endpoints (not JAVBeacon's), which is why both
need a Silo API key configured: `/api/v2/admin/collections` (including members and order),
`GET /api/v2/libraries/unmatched-items`, and
`POST /api/v2/admin/items/{id}/match/apply`.

## Setup

1. Install the plugin on your Silo instance.
2. Add a **JAVBeacon Connection** under this plugin's global configuration:
   - **JAVBeacon URL** - the base URL of your JAVBeacon instance, e.g.
     `https://jav.example.com`.
   - **API Key** - from JAVBeacon's Settings page. JAVBeacon accepts this as
     either an `Authorization: Bearer` header or an `api_key` query
     parameter; this plugin uses the header for API calls and the query
     parameter for resolved image URLs (since those are handed to Silo's own
     image-fetching code, not called by this plugin directly).
3. Add **JAVBeacon** as a metadata provider for the library you use for your
   JAV collection.

## Artwork

This plugin does not resize or transform artwork itself - it passes through
whatever JAVBeacon already serves. Posters use JAVBeacon's
`/covers/{id}/silo-primary` endpoint (a portrait pad/crop of the source
cover, served from Silo's own dedicated route - not Jellyfin's
`/covers/{id}/jellyfin-primary`, even though both apply the identical
transform), since most scraped JAV covers are wide DVD-case wraparound scans
rather than true portrait posters, and a poster grid expects portrait
artwork. Backdrops and screenshots use JAVBeacon's original, untransformed
images. `ResolveImageURL`'s `variant` size hint is ignored: JAVBeacon does
not generate multiple resolutions of an image, so every variant resolves to
the same URL, which the `image_resolver.v1` contract explicitly allows a
plugin to do.

When a release has no JAVBeacon-scraped cover of its own, `GetMetadata`
additionally returns a `stash_screenshot_url` pointing at this plugin's own
dedicated `GET .../releases/{id}/stash-cover` endpoint (new in JAVBeacon
v1.0.239 - previously this plugin had no fallback at all in that case).
`GetImages` adds it as extra poster and backdrop candidates alongside, never
instead of, the regular cover/backdrop entries.

## Dependency Model

This repository consumes `github.com/Silo-Server/silo-plugin-sdk` as a
normal Go module dependency. For local multi-repository development, use a
`go.work` file that points at a sibling SDK checkout; do not commit
machine-local filesystem replacements.

## Development

```sh
go build ./...
go vet ./...
go test ./...
```

## License

No license file is included yet - add one (e.g. matching JAVBeacon's own) before
publishing this repository.
