# Content-aware source reconciliation

This package provides source reconciliation for CACHE-028. An optional typed
controller receiver can use it through the initial owned `HostEnvironment.CopyDir`
call. No production receiver or authority provider is implemented; default checkout
remains ordinary. This does not promote a PR cache or supply a source archive. The caller must own separate writable destination and staging
roots exclusively throughout reconciliation; concurrent mutation is unsupported.

`BuildEnvelope` accepts an explicit tracked source inventory. Its versioned typed
entries cover full SHA-256 content, symlink targets, types and executable bits.
Limits cap entries, metadata and bytes before source allocation. `Bind` records a
full source commit, Git tree and compatible-output identity supplied by trusted
policy. The content digest seals metadata; it does not authenticate those claims.
The transport must independently verify the requested Git tree and writer policy.
Submodules, LFS and sparse-checkout semantics need a transport contract before use.

`Reconcile` validates both envelopes, all staging bytes and destination conflicts
before mutation. Identical entries receive no writes, chmod or timestamp calls,
preserving inode and nanosecond mtime. Changed files use replacement to avoid
writing through donor hardlinks. Deletions apply only to explicit donor leaves;
directory transitions remove only validated empty directories. Unlisted outputs
and Git metadata are protected. The default target directory is always excluded;
callers must declare additional output roots in `Limits.Protected`.

Link chains resolve against the complete requested source inventory and actual
staging/destination state before writes. Directory targets cannot expose unlisted
children. Resolution is bounded and interprets `..` after expanding each alias.

No timestamps are replayed. Changed and added files get normal materialization
times. Operating-system failures during application can leave a partial source
merge; callers must discard such a tree rather than compile it. Cache corruption,
unsupported entries or validation failures must cause a cold authoritative checkout.
Read-only cached compiler outputs are outside the source inventory and merge.

Tests cover identity preservation, changes/deletions/renames, executable modes,
symlink and directory transitions, staging tampering, protected state and bounds.
They run through a pinned Go image under Bosn, never on the developer host.

`ReadFrozenCheckout` reads explicit controller-provided original/effective commit,
Git tree, and Bosn source digest identities from an independent frozen Git store.
Dirty snapshots must bind the synthetic commit and its original parent. Cached
`.git` is never an authority. Git commands, hooks, credentials and fetching are
not used. Loose object headers and inflated byte totals are checked before the
Go-git decoder; packed stores, alternate stores, LFS pointers, submodules and
missing objects currently fail cold.

`PrepareHandoff` separates authenticated source receipts from approved-writer
and materialization grants. The interfaces require a trusted receiver; no
production authority provider is implemented. A candidate's content seal cannot
approve a writer or authenticate an archive. The container preparation method
also binds these receipts to the actual frozen source and private writable
workspace. Preparation is read-only. An explicitly injected receiver may admit only the
initial owned workspace, with independent requested and donor Git authority.
Requested objects and refs are bounded and staged separately before reconciliation;
Git restoration builds safe configuration and a fresh index, excluding source hooks,
filters, remotes and cached Git metadata. Rejected or partially applied donors are
discarded only inside the owned workspace before the actual cold copy. Nested,
unowned and Git-ignore profiles retain ordinary copying.

Real Bosn issuer grants, bounded authenticated archive materialization, compatible
Cargo/Dylint output delivery, and physical PR-to-main promotion remain required
before activation; this API slice does not complete CACHE-028. Receiver test
issuers are fixtures, never production authorization.

The embedding seam is `runner.Config.SourceReceiver`, which is excluded from
JSON/YAML configuration. `startHostEnvironment` passes this typed receiver and
the newly allocated private workspace parent to the actual host executor. CLI
and workflow inputs never create a receiver. Nil retains stock cold checkout,
including the current executable. Source admission validates its own initial
workspace only; it does not change Docker-action stage ownership or tool-store
updater locking.

Bosn activation remains separate: its admitted frozen snapshot and effective Git
tree can identify requested source, but those fields do not grant a cache writer.
A controller-owned payload store must independently approve the donor writer,
policy/workflow/run/attempt/job, namespace and compatible-output identity, bound
and hash the actual materialization, and independently supply donor Git authority.
The trusted embedding boundary must deliver those authenticated objects to the
typed receiver before job checkout. No request JSON or cache seal can construct
that authorization. Existing Bosn source/event copying alone supplies no approved
donor receipt or receiver admission; production activation is not implemented.
