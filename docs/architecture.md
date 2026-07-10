# Architecture

## System boundary

Agent Loop Controller is a deterministic workflow coordinator. It accepts a
normalized trigger, fetches and snapshots the canonical Linear issue, provisions
an isolated workspace, invokes Codex, verifies evidence, and advances a durable
state machine. It is not an LLM agent.

```text
Manual CLI / future Cron / Linear webhook / Hermes
                         |
                    TriggerSignal
                         |
                         v
                 Agent Loop Controller
           admission | state | evidence | gates
              /          |           \
         Linear       Codex Exec      GitHub
                          |
                implementation session
                          |
                  controller verification
                          |
                 fresh Codex review session
                          |
                  PR -> CodeRabbit -> human
```

## Canonical contracts

### TriggerSignal

Trigger adapters submit only an issue identifier, action, requester, timestamp,
and idempotency key. They do not submit authoritative issue contents. The
controller re-fetches Linear to prevent stale or forged task specifications.

### CodingTask

After admission, the controller creates an immutable snapshot containing the
issue goal, acceptance criteria, out-of-scope items, repository, Linear branch
name, repository-owned verifier IDs, policy, and source revision. Linear never
carries executable verification commands. A material Linear edit
after admission creates a human decision point; it never silently changes a run.

### Codex implementation outcome

Implementation is a resumable `codex exec` session. The semantic last message is
validated against `contracts/implementation-outcome.schema.json`. JSONL stdout is
append-only telemetry and stderr is captured separately. Exit code zero alone is
not success.

The JSON schemas constrain the Codex structured-output shape. The controller's
domain validators enforce cross-field semantics, including that a human-decision
status contains a decision request and that a passing review has no findings.
The schemas are embedded into the controller binary and emitted as explicit
preparation artifacts; the runner must materialize them before starting Codex.
Every attempt uses a new empty artifact directory. Schema files are created
exclusively, and each Codex output leaf must still be absent immediately before
the process starts, preventing pre-created symlink output targets.

Resume names the persisted implementation session, runs from the same owned
worktree, and explicitly overrides `sandbox_mode` to `workspace-write` through
the resume command's supported config interface. It never uses `--last`.

### Fresh review outcome

Review is a new `codex exec --ephemeral --sandbox read-only` invocation, never a
resume of the implementation session. Its fixed prompt requires inspection of
the complete `origin/<base>...HEAD` delta. The controller binds the structured
outcome to the exact candidate head SHA and verifies that review did not mutate
the workspace.

Codex CLI 0.144.1's built-in `codex exec review` was not selected for the MVP
adapter because live verification showed that it emitted prose through
`--output-last-message` despite `--output-schema`, and its scope selector could
not be combined reliably with the required custom prompt. This decision can be
revisited after a versioned CLI compatibility test proves structured output.

The fresh reviewer explicitly overrides `sandbox_mode` to `read-only`. Run
artifacts must be outside and non-overlapping with the owned worktree so semantic
output and telemetry cannot pollute the candidate branch. The controller resolves
filesystem identity and physical ancestor directories before enforcing this
separation. Both directories must already exist after provisioning. This lets the
operating system resolve symlinks, case aliases, and Unicode-equivalent paths.
Controller-managed paths also reject raw parent-traversal components.

The Phase 1 runner must record and preflight the installed Codex CLI version and
required flags before executing a plan. Managed commands do not use
`--strict-config`: unrelated stale fields in a user's global Codex configuration
must not prevent an otherwise compatible coding run.

Subprocess stdout and stderr are captured directly to exclusive artifact files,
not duplicated into unbounded memory buffers. Codex session extraction scans
the JSONL artifact as a stream. Only the small version and help outputs are read
through explicit size limits, and capability checks require exact option tokens
from help declaration lines.

Managed runs use `--ignore-user-config` so global MCP servers, hooks, or tools
cannot bypass controller ownership of external side effects. Authentication and
repository instructions remain available; required runtime behavior is supplied
explicitly by the command contract.

The Phase 1A spike runs repository verification once against the uncommitted
implementation before the controller creates a candidate commit, then repeats
the same verifier against the committed candidate. Only the second result is
used as exact-HEAD authorization evidence. This preserves the required ordering
without claiming that a pre-commit result was executed at a later commit SHA.
The spike also treats ignored workspace files as dirty evidence and verifies
that `refs/remotes/origin/<base>` exists and is an ancestor both before
implementation and before fresh review. The reviewed branch delta therefore has
a deterministic Git base and does not depend on uncommitted ignored inputs.
The working branch name is revalidated after every Codex or verifier boundary,
so switching a symbolic branch without changing HEAD cannot redirect the
controller-owned candidate commit.

If review reports findings, the controller resumes the implementation session,
runs verification, creates a new candidate head, and launches another fresh
review. No reviewed SHA may be substituted with a later SHA.

`fresh_review -> pr_open` is deliberately absent from the generic state topology.
The application gate authorizes it only when the review verdict is `pass`, the
reviewed head equals current Git HEAD, and controller verification was recorded
for that same head.

## Review order

1. Codex implementation self-checks while coding.
2. Controller runs repository verification.
3. A fresh Codex reviewer evaluates the entire branch delta.
4. Only a passing internal review allows PR creation.
5. CodeRabbit reviews the PR as the second automated reviewer.
6. Any CodeRabbit-driven code change repeats verification and fresh Codex review.
7. I-Fan approves the exact final head as the final gate.

## Ownership

The controller owns worktrees, branches, commits, pushes, PR creation, retries,
timeouts, state transitions, evidence, merge, and cleanup. Codex edits and tests
inside the assigned worktree and returns structured semantic outcomes. Codex does
not write Linear or GitHub state during implementation.

## Persistence direction

Phase 1B uses SQLite as the authoritative source of local run state. The schema
has explicit ordered migrations and persists runs, ordered transitions, Codex
attempts, head-bound verifications, reviews, and controller-owned resources.
Filesystem artifacts retain full JSONL, stderr, structured outcomes, and verifier
output; SQLite retains paths, hashes, session IDs, exact SHAs, verdicts, and
summaries needed to reject incomplete or mutated evidence.

SQLite schema version 2 adds a compare-and-swap run lease with owner and expiry,
plus digest and size bindings for Codex and verifier stdout/stderr. A local
controller renews its lease while an external process is active and cancels the
operation if ownership is lost. A crashed owner becomes reclaimable after the
bounded lease expiry, preventing concurrent controllers from mutating one owned
worktree. SQLite foreign keys and busy timeout are configured in the driver DSN
so they apply to every physical connection, including connections recreated by
`database/sql`.

The predictable per-run artifact path is reserved in `owned_resources` before
filesystem creation. A random ownership nonce is persisted in SQLite and in an
exclusive marker inside the newly created root. Every start or continue checks
that the root and `attempts` are real directories, remain canonically contained
under the configured run root, and match the marker before reading snapshots or
creating a new attempt. A pre-existing path without the durable reservation is
never adopted.

Run creation is idempotent by immutable issue/source-revision content and only
one active run may own an issue. State transitions use a transaction with an
expected-current-state comparison. External steps are entered from an already
persisted intent state, and implementation/review attempts receive a persisted
row and unique empty artifact directory before process execution. Candidate
commit recovery accepts only the controller's fixed commit identity as the sole
child of the persisted exact base; any other Git/SQLite disagreement fails
closed.

If a controller restarts with a `started` Codex attempt, it does not silently
open a new implementation session. It recovers the explicit session ID from the
attempt JSONL, records the interrupted attempt and session in SQLite, and uses a
new isolated resume attempt. Missing or malformed session evidence stops for
manual handling.

A simulated human decision is immutable evidence as well. Its transition stores
the decision value, file digest, and the exact implementation-outcome path/hash
that contained the offered options. Restart resume revalidates all bindings and
rejects a changed file or a choice absent from the original request.

Verifier adapters return partial evidence together with execution errors. The
controller hashes and persists every check that actually ran, including failed
exit codes, before returning the failure to the state machine. Failed verifier
artifacts therefore remain reachable through SQLite status and inspection.
Authorization groups records by their unique verification evidence path and
selects the latest complete successful batch for the candidate HEAD; older
failed batches remain auditable without permanently blocking a successful
restart retry.

Schema version 3 retains multiple review records for one candidate HEAD. A
transient `failed` verdict may use a new isolated review attempt, while a
`findings` verdict remains a safe stop until a later repair produces a new HEAD.
Authorization considers the latest exact-HEAD review without discarding earlier
review history.

The SQLite adapter uses `modernc.org/sqlite`. Its pure-Go implementation avoids a
CGO compiler/runtime dependency and keeps local and race-test execution
portable. The trade-off is a larger indirect dependency graph and binary than a
CGO-backed SQLite driver. The controller still has only one direct SQLite
dependency and does not shell out to the `sqlite3` CLI.

External events will eventually be at-least-once and deduplicated by request or
event ID. A later reconciler will repair missed or reordered Linear and GitHub
events; Phase 1B deliberately stops at local `approval_ready`.
