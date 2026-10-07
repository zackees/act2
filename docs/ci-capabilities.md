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
  "capabilities": ["qualified-job-identity-v1", "step-stage-result-v1", "selected-job-outputs-v1"]
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
- `selected-job-outputs-v1`: `--json --ci-output <job-path>:<output>` requests
  an interpolated output from that source job path, for example
  `--ci-output precheck/precheck:plan`. Repeat the flag for additional outputs.
  A successful concrete job emits one event with `ciOutputSchema: 1`, its
  existing qualified `jobIdentity`, and `jobOutputs`, an object of requested
  output names to strings. Caller matrix identities stay attached to each
  concrete event; leaf IDs and display names never substitute for the path.
  No flag means no output event. Failed, cancelled, dry-run or cleanup-failed
  executions emit no output evidence. Invalid or duplicate selectors and
  requests without `--json` refuse runner configuration.

  Values containing configured secrets, the token or runtime masks refuse the
  whole payload even with `--insecure-secrets`. Missing requested outputs and
  payloads exceeding 64 KiB or 256 values also refuse it. A refusal emits
  `jobOutputsError` instead of values; it cannot qualify output evidence.
  This is bounded disclosure of explicitly requested outputs, not general
  information-flow tracking: request only non-secret planner outputs.
  Consumers still prove the output producer's source and successful checks,
  reject missing/duplicate/error events, and resolve reusable output mappings
  from the original workflow. An empty string is an output; an absent event
  is missing proof. Planner interpretation belongs to ci-lint, not this event.

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
and the complete local-to-hosted pilot are pending. Released act2.12 supplies
the first two capabilities; selected output evidence is a source candidate
until its compatible release and Bosn receipt consumer are qualified.
