# Local artifact block storage

V4 Azure block uploads are staged by hashed block ID and assembled in the
client's committed order. A retried block replaces that block. Filesystem
access uses Go's rooted filesystem operations; staging-parent symlinks are
rejected, including symlinks whose target remains inside the configured root.

The local service defaults to a 4000 MiB streamed block ceiling and 50,000
staged block IDs across its artifact tree. These preserve Azure's documented
block limits and accept the stock upload-artifact v4 action's 8 MiB chunks.
These bounds are local resource policy, not GitHub artifact quota claims.

The aggregate default is 10 GiB of regular files under the service's artifact
root, including staged blocks and assembled ZIPs. Set
`ACT_ARTIFACT_MAX_TOTAL_BYTES` to a positive integer byte count before starting
the service to select a different budget. Invalid configuration fails closed.
Assembly temporarily retains both blocks and the ZIP, so budget for both.
An overflow returns HTTP 413 and clears the partially written file; successful
retries replace their previous byte accounting. Existing files are inventoried
before the first block request. This budget applies to the V4 block staging and
assembly paths; the older V3 and append-block implementations are unchanged.

Sources: [Azure scalability targets](https://learn.microsoft.com/en-us/azure/storage/blobs/scalability-targets),
[the exact stock v4 uploader](https://github.com/actions/upload-artifact/blob/ea165f8d65b6e75b540449e92b4886f43607fa02/dist/upload/index.js).
