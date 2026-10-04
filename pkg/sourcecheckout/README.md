# Content-aware source reconciliation

This package is an unwired filesystem primitive for CACHE-028. It does not change
act2 checkout behavior, authenticate a cache writer, promote a PR cache, or supply
a source archive. The caller must own separate writable destination and staging
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
