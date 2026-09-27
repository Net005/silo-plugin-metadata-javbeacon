# Changelog

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
