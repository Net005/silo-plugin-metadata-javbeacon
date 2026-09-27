# Changelog

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
