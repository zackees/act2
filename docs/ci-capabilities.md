# CI event capabilities

The standalone CI tool protocol is coordinated in
[zackees/ci.yml#362](https://github.com/zackees/ci.yml/issues/362).
Act2 executes workflows and emits events; Bosn supervises execution and
produces its terminal receipt; ci-lint validates that receipt and owns
attestation. This query does not run checks or create an attestation.

## Query

`act --ci-capabilities` writes exactly one JSON document to stdout and exits.
It reads no workflows, starts no containers or cache servers, and fetches no
version notices. Output failures return an error.

```json
{
  "schema_version": 1,
  "producer": "act2",
  "version": "<the binary's compiled version>",
  "capabilities": ["qualified-job-identity-v1", "step-stage-result-v1"]
}
```

Capabilities identify implemented event contracts, independently of release
version ordering. Bosn must request `--json --verbose` for structured events,
including debug-level skipped-step results. They are not a claim that any particular execution succeeded:

- `qualified-job-identity-v1`: planned concrete jobs carry `jobIdentity`, an
  ordered root-caller-to-leaf list of `{jobID, matrix}` components. Each
  caller's matrix belongs to that caller component. The leaf still supplies
  `jobID` and `matrix`. Display names do not identify an execution. Missing or
  cyclic caller context produces no qualified identity. The existing runner
  conformance tests cover nested calls, repeated display names and caller
  matrices.
- `step-stage-result-v1`: step events carry `stepID`, step name, stage and
  terminal `stepResult`. Consumers distinguish Main from Pre/Post, and actual
  execution from a source-excluded skip. Bosn adds its own event sequence and
  assembles the bounded receipt; act2 does not emit a ci-lint attestation.

## Consumer requirements

Bosn must verify the pinned artifact and extracted binary digest, query that
same binary, validate this schema/producer/version and require its needed
capabilities before workflow execution. An unsupported flag, malformed query,
unknown schema, missing capability or inconsistent pin refuses qualification.
Do not derive capability support merely from a version suffix.

The consumer still validates actual execution coverage and outcomes. This
query supplies no source-tree proof, writer authorization, cache compatibility
or test-pass evidence. Those belong to the enclosing CI protocol.

## Qualification status

The CLI query and output-error tests pass, as do the existing qualified caller
identity tests. A compiled candidate has returned valid JSON with nonexistent
Docker and workflow paths. Bosn consumption, compatible release artifacts,
and the complete local-to-hosted pilot are pending; the current released
act2.10 binary does not gain this interface from a source change alone.
