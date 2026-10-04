# Shared cache retention

This feature branch retains a namespace across fresh act2 servers and expires
individual metadata-backed archives. It is not yet released or pinned by Bosn.

## Configuration

| Flag | Default | Meaning |
|---|---|---|
| `--cache-server-max-bytes` | `0` | Actual completed archive file lengths; zero disables the byte ceiling |
| `--cache-server-max-age` | `720h` | Absolute archive age |
| `--cache-server-unused-age` | `168h` | Time since last lookup/download/upload |
| `--cache-server-gc-interval` | `5m` | Request-triggered and periodic server maintenance interval |

Negative byte limits and nonpositive durations are rejected before store
creation. Incomplete-upload and recent-use grace periods remain five minutes.
The ceiling is per namespace. File lengths and allocated blocks differ;
archive quota bytes exclude metadata, temporary chunks, untracked files and
other cache classes.

## Coordination and retention

Every HTTP cache request holds a shared OS file lock through `transfers.bolt`
for its complete lifetime. GC acquires its exclusive lock before opening
metadata. Active transfers defer collection without consuming the retry
interval. The OS releases locks if a server crashes. Metadata locking alone
would not cover transfers.

Lookup refreshes use metadata before returning an archive URL. Even archives
at their absolute age limit retain the five-minute lookup-to-download grace;
active transfers remain protected beyond that grace.

After age cleanup, the byte pass measures actual archive lengths and removes
least recently used completed entries until the ceiling is met. Recently
used entries are protected; warnings report unmet ceilings. Unknown sizes,
nonregular archives and overflow stop the byte pass. Failed removal preserves
metadata for accounting and retry. No pass removes the whole store. A periodic
timer maintains an idle running server and stops when its handler closes.

## Verification

- RED: two 80-byte completed archives younger than the existing age limits
  remained above a 100-byte ceiling. GREEN: the older archive expires while
  the newer archive stays warm.
- RED: GC unlinked an archive during another server's download. GREEN:
  transfer protection works across separate OS processes, and expiration
  resumes after the download finishes.
- The artifactcache suite passes under the race detector. Tests also cover
  idle-server maintenance, real lengths instead of declared upload sizes,
  invalid policy rejection before creation, and metadata preservation on
  failed filesystem removal. CLI policy parsing has a focused test.
- The full CLI suite passes against a private Docker 29.7.2 test daemon with
  RAM-backed storage. This also caught direct test-input fixtures lacking the
  new typed policy defaults; those fixtures now initialize the same default
  policy as the CLI. No host Docker socket or host cache was used.
- The changed artifactcache and CLI packages pass the repository's pinned
  golangci-lint v2.11.4 configuration. End-to-end Bosn workflow proof remains
  separate from these package checks.

## Integration still required

All servers sharing a namespace must use this protocol. Legacy act2 servers
do not participate. Do not enable quota against mixed-version live stores
without a verified exclusion mechanism.

## Offline namespace operations (local candidate)

`cache audit --cache-server-path PATH --cursor ID` exports typed JSON without
starting a server or creating a missing store. Pages contain at most 12 entries.
Compare fingerprints across pages: they cover metadata and file sizes, not
archive contents. Missing, ready, busy and partial states are distinct; unknown
sizes are null. Row/file counts, metadata size and operation time are bounded.
The audit holds an exclusive coordination lock using a read-only descriptor.
Verified on a disposable Docker volume mounted read-only: an attempted write
fails with EROFS, the audit reports both archives (160 bytes), and metadata stays
byte-identical.

`cache prune --cache-server-path PATH` only inspects the store. `--apply` applies
the configured age/byte policy under the same lock, even when no server runs.
Partial inventories, including untracked files, refuse collection. These commands
require the new coordination file; legacy or mixed-version stores are unsafe for
automatic eviction. Audit archive bytes include regular temporary/untracked files
but exclude metadata, directory overhead and allocated-block differences.

RED: a closed server left 160 archive bytes under a 100-byte policy. GREEN:
offline maintenance leaves the newest 80-byte archive. Audit pagination tests
verify unchanged metadata, consistent pages and missing/busy/corrupt states.
The updated artifactcache suite passes the race detector; focused offline CLI
and policy tests pass. The updated packages pass pinned golangci-lint v2.11.4 (zero issues).
The full CLI Docker suite result above predates this slice.

Applied maintenance reports successful deletion counts, completed-archive bytes
reclaimed, and up to 32 receipts with exact entry IDs and typed reasons. Omitted
receipt count is explicit. Failed file or metadata removal emits no successful
receipt. The byte pass reports remaining completed bytes, recently used protected
bytes and whether its ceiling was met; a partial/aborted pass leaves those fields
unknown. These values exclude temporary data and filesystem allocation overhead.
Tests cover both successful byte eviction and a protected over-budget namespace.

Aggregate machine budgeting, sustained warm-run benchmarks and Bosn integration
remain open. The Bosn pin still selects released act2;
this branch authorizes no host-cache deletion.
