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
