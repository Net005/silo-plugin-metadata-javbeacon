# Changelog

## [0.4.15] - 2026-09-27

### Fixed

- Run collection synchronization inside the scheduled-task RPC too, so Silo reports real success or failure instead of treating a detached goroutine as a completed task.

## [0.4.14] - 2026-09-27

### Fixed

- Run auto-match inside the scheduled-task RPC in bounded batches and report actual matched, skipped, and failed counts instead of immediately reporting a detached background job as completed.

## [0.4.13] - 2026-09-27

### Fixed

- Fetch Silo collection order ETags and send `If-Match` when reordering members, as required by the live v2 API.

## [0.4.12] - 2026-09-27

### Fixed

- Use Silo v2 collection list without an unsupported `limit` query parameter; validated against the live server.

## [0.4.11] - 2026-09-27

### Changed

- Sync the StashApp Watchlist and JAVBeacon saved filter sets as ordered Silo v2 collections, using local matched media only. Reconcile additions, removals, and order through a one-minute background poll and the scheduled collection task.
- Stop publishing Watchlist and saved filter membership as metadata genre tags.

## [0.4.10] - 2026-09-27

### Fixed

- Auto-match Stash-only scenes by exact local filename when JAVBeacon has
  no release. Send both JAVBeacon's stable `stash:<scene-id>` provider ID
  and the Stash scene ID to Silo's match/apply API; preserve ambiguity checks
  across all files attached to an item.

## [0.4.9] - 2026-09-27

### Changed

- Preserve StashApp's newest-first Watchlist order and update timestamps
  during Silo watch sync, including distinct Stash-only scenes.
- Refresh Stash-only media and prior Watchlist members when their Stash tag
  changes, and avoid duplicate or stale Watchlist genre labels.

## [0.4.8] - 2026-09-27

### Fixed

- Refresh Silo metadata for JAVBeacon Watchlist members during collection
  sync, including when there are no saved filter set memberships. This
  applies the Watchlist genre tag to already-matched releases.

## [0.4.7] - 2026-09-27

### Changed

- In auto-match, read every Silo media file path and search by each actual
  filename before falling back to a parsed title when no file is available.
  Leave items unmatched if their files resolve to different provider IDs.

## [0.4.6] - 2026-09-27

### Added

- Match StashApp scenes by exact filename when JAVBeacon has no release,
  including scenes without a code. Fetch Stash-only metadata and images by a
  stable scene ID and keep ambiguous filenames unmatched.

## [0.4.5] - 2026-09-27

### Fixed

- Skip JAVBeacon searches for ordinary titles in the unmatched-items task;
  only release-code titles can pass its exact-match check.

## [0.4.4] - 2026-09-27

### Added

- Enrich Silo performers with StashApp birth dates when available and a JAVBeacon homepage redirect to the StashApp performer page.

### Changed

- Cache and coalesce repeated release metadata requests made during Silo scans.
- Start collection sync and auto-match as bounded background jobs so Silo's short scheduled-task RPC deadline no longer stops the scan.

### Fixed

- Keep failed collection refreshes pending for retry and report background errors in plugin logs.
