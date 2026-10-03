# Changelog

## [0.4.45] - 2026-10-03

### Fixed

- Forward watched events for Stash-only Silo matches when their scene ID is stored in the `javbeacon` provider ID or prior remote-state key. Use the event ID as a stable session fallback and preserve the source playback time.

## [0.4.44] - 2026-10-02

### Improved

- Separate unmatched matching, matched-item metadata repair, incremental metadata refresh, watched-state import, and collections into independently runnable scheduled tasks. Keep their resident polling for fast updates and prevent overlapping runs of each task. A long unmatched scan no longer delays missing-cast repair.

## [0.4.43] - 2026-10-02

### Fixed

- Repair matched Silo items whose scan left cast or other metadata empty. Check the newest items on each auto-match run while paging through older matches; reapply only an exact release code tied to the same Stash scene. Include Stash-only scenes.

## [0.4.42] - 2026-10-01

### Improved

- Check Watchlist and selected JAVBeacon and Stash saved-filter membership every 30 seconds, reconciling existing collections as soon as the source changes. Skip the expensive Silo catalog scan when membership is unchanged, and retain a full pass every five minutes plus the scheduled task to catch local library changes.

## [0.4.41] - 2026-10-01

### Fixed

- Acknowledge scheduled collection sync within one second so Silo does not mark the task failed while the resident worker pages through all enabled movie libraries. The worker continues reconciliation and logs failures.

## [0.4.40] - 2026-10-01

### Fixed

- Discover all enabled Silo movie libraries whose provider chain enables JAVBeacon when syncing Watchlist and saved-filter collections. Match each collection only to local items in that library instead of placing every Stash collection under the configured JAV library.
- Remove plugin-owned per-library collections when that library has no matching members. Preserve administrator-created collections and source ordering.

## [0.4.39] - 2026-10-01

### Added

- Separate JAVBeacon Release Library and StashApp scene saved-filter selection and collection prefixes. Preserve the existing JAVBeacon settings, import selected Stash filters through the new JAVBeacon API, and match Stash members by exact local file path. Stash import is opt-in; changing either selection reconciles only plugin-owned collections for that source. Requires JAVBeacon v1.0.274 or later for Stash filter import.

## [0.4.38] - 2026-10-01

### Added

- Select Stash saved filter sets for Silo collection import by exact name or numeric ID in plugin settings, and add a custom prefix to imported collection titles. A blank selection preserves import-all behavior; Watchlist remains independently synchronized. Deselecting removes only the plugin-owned collection for that filter.

## [0.4.37] - 2026-09-30

### Improved

- Keep auto-match running in a resident worker after the scheduled-task response, check for new unmatched files every minute, and process up to 400 matches per pass. Extract leading release codes from filenames with quality suffixes, normalize hyphen/underscore variants, and choose a stable record when duplicate exact codes have equal metadata.
- Coalesce near-simultaneous library snapshots and stop clearing the metadata cache on every watched-state poll.

### Fixed

- Route each changed Stash/JAV file to its actual Silo movie library, then wait for each asynchronous item-refresh job to succeed before acknowledging JAVBeacon metadata changes. Submit at most 50 jobs per poll and retry failed jobs with backoff instead of silently losing edits after a job fails. Requires JAVBeacon v1.0.272 or later.

## [0.4.36] - 2026-09-30

### Fixed

- Import Stash watched state across every matching Silo movie library, including Hentaied and Other, instead of only the configured JAV library. Match exact local file IDs before titles. Run watched reconciliation independently every 30 seconds so collection rate limits do not block it; process up to 200 newly watched items per pass.

## [0.4.35] - 2026-09-30

### Improved

- Poll JAVBeacon’s local metadata change feed every 15 seconds independently of collection sync. Resolve changed Stash and JAVBeacon files to exact Silo library items, clear stale provider cache, and queue targeted complete refreshes instead of rescanning the entire library. Verify fallback catalog matches against the actual file path and retain the change cursor when Silo rejects a job. Requires JAVBeacon v1.0.272 or later.

## [0.4.34] - 2026-09-29

### Fixed

- Import watched state for Stash-only local movies by matching either their file name or Stash scene title. Skip ambiguous titles to avoid marking the wrong movie watched. Continue reconciling every minute, and preserve Stash play counts in the watch snapshot.

## [0.4.33] - 2026-09-29

### Fixed

- Display the Stash scene title for Stash-only Silo movies instead of the filename-derived matching code. Keep the code as the provider identity and fall back to it only when Stash has no scene title.

## [0.4.32] - 2026-09-27

### Improved

- Fill Silo performer biographies with factual Stash profile fields when Stash has no written biography. The background updater fetches each performer profile once per job, patches only known birth dates, and recognizes reversed names by their Stash portrait ID.

## [0.4.31] - 2026-09-27

### Fixed

- Recover a Stash performer ID from an existing person portrait URL when Silo supplies it during person refresh. The returned detail includes the stable identity, allowing later refreshes to use it directly. Requires Silo core to pass the stored portrait source on legacy people.

## [0.4.30] - 2026-09-27

### Fixed

- Use JAVBeacon’s cropped Stash fallback poster separately from the original scene image used for Silo backdrops. Avoid duplicate image candidates when the poster and backdrop are already primary artwork.

## [0.4.29] - 2026-09-27

### Fixed

- Match Stash performers to Silo people when first and last names are reversed, verifying the Stash ID against the portrait URL before updating birth date and homepage.

## [0.4.28] - 2026-09-27

### Fixed

- Return real Stash performer metadata from Silo's `GetPersonDetail` refresh RPC instead of an empty result. Cast entries carry a namespaced Stash performer identity, allowing person refresh to fetch current birthdate, biography, homepage, and portrait from JAVBeacon. Existing people gain that identity on their next full release metadata refresh.

## [0.4.27] - 2026-09-27

### Fixed

- Reapply Stash performer birth dates and homepage links during Silo metadata refreshes. Replace the 24-hour person suppression with a short per-person cooldown and one trailing update, so a full refresh cannot leave an existing person stale after earlier cast writes.

## [0.4.26] - 2026-09-27

### Fixed

- Accept a Stash scene ID supplied in Silo's `stash` provider ID field during manual search and metadata retrieval. A directly selected Stash scene now resolves even when JAVBeacon's release text search has no match.

## [0.4.25] - 2026-09-27

### Fixed

- Continue collection reconciliation in the resident worker when Silo's hard 10-second scheduled-task RPC expires. The task returns a running status while the one-minute poll keeps updates moving; each pass can reconcile up to 400 changes.
- Mark matching local Silo items watched from the Stash/JAVBeacon snapshot, skipping items already played. Silo's generic watch-provider importer cannot match JAVBeacon/Stash provider IDs, so stop advertising inbound watch import there; active playback scrobbling remains enabled.
- Match Watchlist entries by release code when a snapshot entry has no file path.

## [0.4.24] - 2026-09-27

### Fixed

- Use release codes supplied in JAVBeacon v1.0.259's Silo library snapshot when mapping saved-filter collections, avoiding thousands of individual metadata requests after filters begin returning local items. Retain a paginated fallback for older JAVBeacon versions.
- Yield a partial collection batch before the scheduled-task deadline instead of failing after work has begun.

## [0.4.23] - 2026-09-27

### Changed

- Rotate Silo collection artwork every six hours instead of every three days.

## [0.4.22] - 2026-09-27

### Added

- Rotate Silo collection posters and backdrops every three days using artwork from that collection's local members. Most picks favor recent releases, while some draw from the whole collection. Upload through Silo's artwork API and remember the chosen member IDs to avoid repeated uploads during routine syncs.

## [0.4.21] - 2026-09-27

### Changed

- Keep JAVBeacon-managed Silo collections alphabetized in the library collection list while preserving other collections' positions.
- Raise collection reconciliation to 120 changes and auto-match to 100 matches per run. Give auto-match up to 20 seconds, while still yielding on Silo rate limits or a nearing task deadline.

## [0.4.20] - 2026-09-27

### Fixed

- Give collection sync enough time to read the authoritative StashApp Watchlist snapshot and reconcile a bounded membership batch. Auto-match keeps its shorter work window.

## [0.4.19] - 2026-09-27

### Fixed

- Sync collections from Silo’s paginated local catalog for a configured library, avoiding the RuntimeHost media-list callback that times out in scheduled tasks. Reconcile up to 40 changes per run and resume on later runs.

## [0.4.18] - 2026-09-27

### Fixed

- Allow a configured Silo URL for scheduled tasks, avoiding a RuntimeHost.GetHostInfo callback that times out during live task execution.

## [0.4.17] - 2026-09-27

### Fixed

- Stop marking the intentionally blank, redacted JAVBeacon API key field as invalid when a secret is already saved. Connection tests still verify that a usable key exists.

## [0.4.16] - 2026-09-27

### Fixed

- Route Silo fully qualified task keys to the correct scheduled task. Stop auto-match batches at 40 matches or when Silo rate limits requests, preserving the cursor for the next run.

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
