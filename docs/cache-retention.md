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
Archive inventory reads 256 directory entries per page, with a shared 100,000-entry
ceiling and a 64-directory depth ceiling; it checks cancellation between pages
and entries without materializing an entire directory first.
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

Deletion first commits a typed intent containing the cache identity and reason.
After file removal, one transaction deletes both metadata and intent. Interrupted
removal can therefore be retried: audit reports partial while an intent remains,
and maintenance accepts a missing archive only when its intent matches. Unknown
missing archives still refuse collection. Cohort recovery waits until every
namespace passes preflight. Recovery reports zero newly reclaimed bytes for an
already absent archive and emits no duplicate receipt on the next pass. Tests
cover interruption followed by lookup, unrecorded missing files, and a partial peer deferring cohort
recovery. The full cache package passes race testing; Go lint reports zero issues.

Aggregate machine budgeting, sustained warm-run benchmarks and Bosn integration
remain open. The Bosn pin still selects released act2;
this branch authorizes no host-cache deletion.

## Automatic aggregate maintenance on shutdown (local candidate)

`--cache-server-cohort-max-bytes BYTES` opts into aggregate maintenance when
the cache server closes. It requires `--cache-server-cohort-root ROOT`, refuses
negative values and defaults to zero (explicit aggregate maintenance only).
Shutdown stops namespace maintenance and the HTTP server, releases the server's
shared root lease, then attempts the existing five-second aggregate pass. Live
peers or transfers defer the pass. The last normal server shutdown can reclaim
idle namespaces without an external prune command. Recently used archives keep
their grace period and may leave an explicitly reported protected overage.

`RetentionOnClose()` exposes the typed outcome, also emitted as the
`cache_retention` structured log field. Deferred/incomplete maintenance does not
turn workflow cleanup into a failure. Repeated `Close` does not run a second
pass. `CloseContext` propagates a caller's cancellation/deadline; the CLI uses
an uncancelled cleanup context while the maintenance deadline remains bounded.
Function-exit cleanup covers ordinary execution, watch exit and early errors.

RED: 240 bytes remained after shutdown under a 160-byte aggregate ceiling.
GREEN: eight cycles add cold archives and start fresh servers; each restores
the warm 80-byte archive and shutdown leaves at most 160 completed archive bytes
without an explicit prune. Active-peer deferral, last-peer retry, protected
overflow, cancellation and lease release are covered. The cache race suite and
targeted CLI tests pass; lint reports zero issues for both packages.

This is normal-shutdown maintenance. A killed server cannot run it; deferred
or protected work still needs a host maintenance retry. Allocated blocks,
metadata, legacy retained sources and other cache classes remain outside this
completed-archive ceiling. Bosn's automatic policy wiring and warm cutover
remain open, and its released act2 pin does not yet expose these flags.

## Aggregate retention (local candidate)

Servers can opt into `--cache-server-cohort-root ROOT`; their namespace must be
a direct child of ROOT. Each server holds a shared root lease until it closes.
Existing legacy namespaces are refused rather than implicitly enrolled. Cohort
namespaces also refuse candidate servers that omit the root. Use a new directory
cohort that the released Bosn/act2 pins never address. The marker is a protocol
identity, not protection against arbitrary legacy programs writing that path.
A bounded completed-archive import is implemented below. Bosn rollout and
quiescence verification remain required.

`cache prune-cohort --cache-server-path ROOT --max-bytes N --apply` holds the
exclusive root lease, excluding live servers and new namespace creation. It locks
and validates every namespace before any deletion. Busy, unknown, legacy, partial
or oversized inventories refuse the pass. The directory page is bounded to 64
namespaces; callers must not interpret a refused oversized cohort as empty.
The pass first applies namespace age/byte policy, then evicts eligible archives
in global UsedAt order with deterministic namespace/ID ties. Recent-use protection
still applies. Per-namespace receipts distinguish aggregate-budget eviction.
The report includes remaining completed bytes, protected bytes and budget outcome.

Verification: two namespaces each within a 160-byte local ceiling leave 320 bytes
under namespace maintenance alone; aggregate maintenance retains the newest 160
bytes in the warmer repository. An unknown file in the last namespace prevents
any removal in the first. A live server excludes aggregate maintenance; an
exclusive root lease prevents new namespace creation. Tests also cover protected
aggregate overflow, legacy refusal and absent-root noncreation. The full
artifactcache race suite passes (16.77 seconds), focused offline CLI/policy tests
pass, and pinned golangci-lint v2.11.4 reports zero issues. Bounded directory
reads initially exposed nondeterministic report ordering; namespaces now sort by
name before validation, and the global eviction order has explicit stable ties.

This policy bounds completed archive lengths only. Metadata, temporary/untracked
files, allocated blocks, other cache classes and host image/build-cache storage
still require Bosn accounting and separate retention. The root exclusion policy
is conservative: any live server defers aggregate collection, even when idle.
Bosn wiring, scheduled retry, automatic warm cutover and sustained concurrent
workload validation remain open. Nothing enables eviction on the existing host
cache or changes the released Bosn pin.

## Warm legacy import (local candidate)

`cache import --cache-server-path NEW_ROOT --from LEGACY_NAMESPACE
--namespace NAME --max-bytes N --apply --source-quiescent` copies completed
archives into a new cohort namespace. Stop legacy servers first: the source
read-only metadata lock blocks legacy GC/metadata writes, and can make live
legacy requests fail while it is held. Quiescence is a caller contract; the flag
is an acknowledgement, not automatic evidence that servers are stopped. Bosn
must establish that evidence before automated migration.

The importer never mutates source metadata, files, retention timestamps or
coordination files. It copies the most recently used completed archives that
fit the positive byte bound, reporting skipped incomplete/over-budget entries.
Original creation/use timestamps survive, so migration does not reset age policy.
It validates exact lengths and SHA-256 of source/destination, then rechecks the
source inventory fingerprint. Checksums detect observed file changes; they are
not coordination with arbitrary legacy writers outside their metadata protocol.

An exclusive cohort lease excludes candidate servers during staging. Files are
synced, metadata closes, then the namespace is atomically renamed and the root
directory synced. Existing destinations are refused. Failed staging deletes only
this invocation's generated directory; cleanup failure keeps an explicit pending
stage path. No unpublished copy emits successful import receipts. The 60-second
operation bound is checked between I/O reads; filesystem calls can still block.
At most twelve import receipts are emitted; omitted count is explicit.

Verification: a fixture without transfer coordination imports unchanged metadata
into the new cohort; a fresh server performs an actual HTTP cache lookup/download
hit. New reservations allocate IDs beyond imported entries. Tests cover byte
bounds, refusal to overwrite, cancellation/corrupt source and an archive changing
after inventory: no namespace is published, staging is cleaned and source remains.
The full artifactcache race suite and focused offline CLI/policy tests pass;
the final migration-specific race check also passes. Pinned golangci-lint v2.11.4
reports zero issues for the updated artifactcache and CLI packages.

This is copy migration, so legacy bytes remain retained and duplication must be
included in rollout headroom. Automatic Bosn quiescence, orchestration, retry and
cutover are open. The new cohort is not enabled on the host; no legacy host cache
was copied or removed by these tests.

Repeated-workload verification combines import, fresh HTTP servers and aggregate
maintenance for eight cycles. Every fresh server restores the imported 80-byte
archive. Each cycle adds 160 bytes in a cold namespace; active-server maintenance
defers, then idle maintenance returns completed archives to at most the 160-byte
aggregate ceiling without evicting the warm input. A final fresh server still
restores it. This and the original import-hit test pass under the race detector
(1.176 seconds); pinned golangci-lint reports zero issues. These are handler/file
operations in an isolated test container, not Bosn run-container orchestration.
Namespace metadata overhead and retained legacy-source bytes are outside the cap.

## Supervised aggregate retry (local candidate)

`act cache prune-cohort --apply --cache-server-path ROOT --max-bytes BYTES
--watch 1m` runs immediately, then waits the interval after each bounded pass.
It emits one JSON CohortReport per pass. Busy, missing, unreadable or partial
stores remain visible and are retried instead of terminating the watcher.
One-shot invocation retains its nonzero exit on a partial report. Explicit
zero or negative watch intervals, invalid policies and negative budgets are
rejected before maintenance. Context cancellation stops further passes;
output failure terminates the command so a supervisor can detect it.

The watcher uses the same exclusive root lease and namespace preflight as
one-shot maintenance. It can maintain enrolled idle stores after their servers
crash, and retries when a live server prevents collection. It does not start
cache servers, create missing roots, enroll legacy namespaces, or infer that
legacy writers are quiescent. A supervisor must start and monitor it; Bosn
integration and safe warm migration remain open.

RED: the command rejected --watch, preventing any periodic retry. GREEN:
focused CLI tests under the race detector verify three incomplete passes,
invalid intervals, cancellation before the first pass, and actual busy-to-idle
root lease recovery using a running handler. An empty store lease test proves
retry coordination; aggregate byte eviction remains covered by the separate
cohort workload tests.

The watcher workload test seeds three completed 80-byte archives through the
real storage and metadata implementations, with shutdown retention disabled.
No-byte-policy maintenance leaves 240 bytes. The watched CLI with an 80-byte
budget removes the two eligible old cold archives and preserves the recently
used warm archive. A fresh server then serves that key and its exact bytes via
real loopback HTTP lookup/download. This passes under the race detector and
pinned lint. It proves idle archive convergence independently of shutdown
retention; it does not simulate SIGKILL or Bosn process supervision.

The CLI watcher now uses the existing EarlyCancelContext bridge so the first
Ctrl+C (graceful job cancellation) stops maintenance, as does SIGTERM/forced
cancellation. A focused RED test originally remained running until force
cancellation; GREEN stops after the separate job context is canceled while
the force context stays live. The focused CLI race suite and lint pass.

## Warm import headroom admission (candidate)

Before copying archives, import now measures caller-available free bytes on
the destination staging filesystem. Linux/macOS use available filesystem
blocks; Windows uses GetDiskFreeSpaceEx. An unreadable probe or unsupported
platform refuses import. The estimate adds selected archive lengths, a 64 KiB
allocation cushion per archive, and 64 MiB metadata headroom. Insufficient
space refuses copying and removes this invocation's stage; source data and an
existing destination remain protected.

ImportReport exposes `retained_source_archive_bytes`,
`available_destination_bytes` and `required_additional_bytes`. Retained source
archive lengths are separate from the copy estimate: current free space
already reflects their allocation when source and destination share a
filesystem. These values are apparent bytes/estimates, not a combined physical
footprint or a reservation. Concurrent writers, unusual allocation overhead,
quotas and a host-wide free-space floor still require Bosn admission policy.
No automatic source deletion is introduced.

Focused RED cases for missing refusal/accounting now pass under the race
detector. Actual failure-path tests preserve source metadata and archives and
leave no published namespace or stage. Existing successful import tests check
retained source bytes and a native destination probe. Windows amd64 and macOS
arm64 package cross-builds pass. The existing host CI selection now includes
`TestImport*` so native adapters run there; that new CI result is pending.

Windows publication uses a same-volume MoveFileEx call with WRITE_THROUGH,
without replacement of an existing target, after files/metadata close. It
avoids Unix read-only directory fsync, which fails on a Windows directory
handle. Unix publication retains rename followed by parent-directory sync;
a sync failure preserves the truthful published state. Microsoft's
[directory-move example](https://learn.microsoft.com/en-us/windows/win32/fileio/moving-directories)
uses this Windows API/flag combination. Native import CI remains required;
this is not proof of power-loss durability on arbitrary Windows filesystems.

## Native Windows coordination correction (candidate)

CI run 37182945345 at act2 `0ae797d` passed full Linux tests, macOS
import tests, lint and snapshot builds. Windows import tests failed before
publication: bbolt attempted to truncate an intentionally read-only
coordination descriptor while acquiring an exclusive lease.

Windows now validates the existing bounded coordination database read-only,
then takes a native exclusive LockFileEx lease over exactly bbolt's lock byte.
Shared leases retain bbolt's read-only locking protocol. No writable descriptor
or creation is permitted by inspection. Close releases the lock and handle
idempotently; acquisition failure returns a nil lease. Unix retains its
existing bbolt protocol.

Read-only file preservation, shared/exclusive exclusion, separate-process
transfer protection and import tests pass with the race detector in isolated
Linux Docker (2.124 seconds); pinned lint reports zero issues. The previous
full artifactcache race suite passed in 16.844 seconds after correcting a
typed-nil lease regression. Windows package cross-build passed. Existing host
CI now selects the lease and separate-process transfer tests as well as import
tests. Review found no concrete protocol defect; native Windows execution of
this correction remains pending. This does not complete Bosn policy wiring,
maintenance supervision, warm cutover or machine-wide physical budgeting.

## Command-level warm-cutover verification (candidate)

The integration test `TestCacheCutoverCommandsPreserveFreshServerHitsAndRepositoryIsolation`
uses actual loopback HTTP reservation, upload, commit, lookup and download. Two
stopped source servers have the same cache key/version but different 80-byte
payloads, under separate 16-digit repository identities. Cobra import commands
copy each store into `actcache/cohort-v1/<repository hash>` with an 80-byte import
bound; source metadata remains byte-identical. Three successive fresh servers
per repository download the correct distinct payloads after import.

A command-level aggregate pass with an 80-byte ceiling correctly reports
160 remaining bytes, 160 protected bytes and BudgetMet=false: both archives
were recently transferred. This verifies truthful protected overflow, not age
expiry or sustained convergence. Linux Docker race testing and pinned command
lint pass. Servers have immediate failure-path cleanup plus explicit close
before import. The existing native host CI selection includes this test; no
workflow/job/runner was added.

Production integration still requires a verified released binary pin, typed
machine policy, durable routing/cutover state, source quiescence across daemons,
and supervised maintenance independent of run teardown. Proposed command sequence:
`act cache import --apply --source-quiescent --from SOURCE --namespace HASH
--max-bytes IMPORT_BOUND --cache-server-path COHORT`; start servers with
`--cache-server-path COHORT/HASH --cache-server-cohort-root COHORT`, explicit
namespace byte/age policy and optional close-time aggregate policy; supervise
`act cache prune-cohort --apply --watch 1m --max-bytes AGGREGATE_BOUND
--cache-server-path COHORT` with the same namespace byte/age policy.
The source-quiescent flag is caller responsibility, not a detector of old
peers. This test does not authorize automatic source deletion or prove those
Bosn daemon paths are implemented.

## Refuse an empty warm cutover under an undersized budget

Import previously published an empty destination when every completed source
archive exceeded the import ceiling. This breaks warm migration and prevents
a later retry because the destination already exists. A focused regression
with two 80-byte archives and a 79-byte ceiling reproduced that publication.
Import now refuses before staging/publication when completed archives were
skipped for budget and none were selected. The report is partial with an
actionable budget error; source metadata and archives remain untouched. A retry
with an 80-byte budget can publish one warm archive at the same destination.
An actually empty source remains eligible for empty initialization. This does
not establish caller quiescence or Bosn rollout. Import race tests passed
(1.177 seconds), including actual loopback HTTP restore after the larger-budget
retry and genuinely empty-source initialization. Pinned lint reported 0 issues.
This correction is not yet released; act2.5 retains the previous behavior.

## Import publication receipts (candidate, not released)

Import now writes `import-receipt-v1.json` inside its private stage before
namespace publication. The bounded typed record includes absolute source and
destination identities, source inventory fingerprint, selected byte ceiling,
retained-source archive bytes, imported counts/bytes and at most twelve archive
checksum receipts with an explicit omission count. The receipt file is synced
and closed before namespace rename. Failed receipt creation prevents publication
and follows the existing private-stage cleanup path.

`cache import-receipt --cache-server-path COHORT/NAMESPACE` reads this historical
evidence without creating or modifying a store. It refuses unsupported cohort
identity, missing/nonregular/oversized input, wrong destination, unknown fields,
trailing JSON, invalid counts/bytes and invalid or duplicate archive receipts.

The receipt records verified creation-time import, not current archive inventory,
successful parent-directory sync, peer exclusion or deletion authority over the
source. It can survive publication with a lost stdout acknowledgement. Recovery
must still reconcile the destination and current archive state before enrollment;
absence or invalid evidence cannot authorize overwriting an existing namespace.
No full power-loss durability claim is made by this slice.

RED: successful import left no receipt for a lost acknowledgement. GREEN: the
receipt is published with the namespace. An actual subprocess withheld its
report after publication, was terminated by its parent, and left a readable
receipt plus an HTTP-restorable archive; source metadata stayed byte-identical.
Import race tests passed (1.118 seconds), and the actual Cobra cutover/receipt
command test passed (0.028 seconds). Pinned lint reports zero issues. Bosn
production enrollment and supervision remain separate integration work.

The subsequent full artifactcache race suite passed (17.031 seconds); focused
offline and cutover Cobra tests passed (0.034 seconds). Independent review
found no blocking issues. Native full CI remains required before release.
