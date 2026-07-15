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
              admission and durable run state
                         |
                         v
                long-lived delivery driver
             next action | evidence | gates
              /          |            \
         Linear       Codex Exec       GitHub
                          |               |
                implementation/review   PR -> required CI -> I-Fan approval
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

The automatic worker is the production admission adapter. It scans only
configured IFAN Todo candidates, then reserves and moves exactly one eligible
issue to In Progress before handing it to the durable driver. `controller run
IFAN-xxx` remains a bounded recovery or local-lab adapter. Both paths fetch the
same authoritative source and apply the same eligibility rules: the issue must
be in `Todo`, an active current cycle, team `IFAN`, labelled `agent:codex` (and
not `agent:hermes`), and contain exactly one label that maps to a
controller-owned repository profile. The issue description must have a
`## Goal` or `## Outcome` section and a `## Acceptance Criteria` section;
`## Out of Scope` is preserved when present. Verification always comes from the
matched repository profile, never from Linear text. A repeated trigger with the
same immutable source resumes the same run. A material source, branch, or
repository change sends the active run to `manual_intervention` for a human
decision rather than creating another active run or rewriting its snapshot.

### Repository registry

Registry version 1 selects one repository by case-insensitive canonical
`owner/name`. Each concrete entry binds a non-symlink local checkout, artifact,
and worktree roots plus either a local bare fixture origin or a credential-free
canonical GitHub origin URL. The GitHub URL must name the same `owner/name` as
the registry entry; checkout transport may be SSH or HTTPS without changing
that identity. The entry also binds `builtin:v1` verifier policy; base branch; a
non-secret GitHub App profile reference; installation and immutable repository
IDs; and allowed operator logins. Duplicate identities, shared or overlapping
paths, unsupported verifiers, and incomplete legacy entries fail closed.

Controller configuration versions 2 and 3 store these repository entries inline
in the one operator configuration document. Version 1 configurations retain the
separate registry-file reference only for compatibility; the registry validator
and per-run authority evidence are the same in both forms.

SQLite schema version 8 freezes a stable `repository-profile:<owner>/<repo>` ID,
the profile snapshot schema version, canonical credential-free JSON, and its
SHA-256 digest for every new run. The snapshot binds repository and base branch,
verifier policy, immutable GitHub App ID and profile identity, installation and
repository IDs, and trusted operator database/node/type identities. JSON source field order cannot affect the
canonical digest. Full local paths remain in the private binding needed to
reproduce and protect controller-owned resources; status and inspect expose only
the sanitized profile identity, policy references, and digests.

Restart rejects any change to the selected profile snapshot or its source,
artifact, worktree, or origin authority binding, while unrelated repository
entries may change without invalidating the run. GitHub App private-key or installation-token rotation does
not change the profile when the configured App identity, installation, and
repository identities are unchanged. Rows created before schema version 8 keep
empty profile-evidence columns and fail closed because migration cannot prove
their original authority binding.

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

The model is controller-owned policy, not task input or Codex configuration.
Implementation and every explicit resume request `gpt-5.6-terra`; a fresh,
independent review requests `gpt-5.6-sol`. The implementation model and session
ID are persisted together, and resume fails closed if either is missing or if
attempt evidence conflicts. Review remains on Sol until representative
evaluation demonstrates that Terra provides equivalent review quality.

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
5. Required CI evaluates the PR at its exact head.
6. Any code change repeats verification and fresh Codex review.
7. I-Fan approves the exact final head as the final gate.

## Ownership

The controller owns worktrees, branches, commits, pushes, PR creation, retries,
timeouts, state transitions, evidence, merge, and cleanup. Codex edits and tests
inside the assigned worktree and returns structured semantic outcomes. Codex does
not write Linear or GitHub state during implementation.

## Delivery driver and human gates

The worker normally admits the durable run and starts its long-lived delivery
driver. `controller drive <run-id>` starts the same driver for an already
admitted run after a process or host restart. `controller run IFAN-xxx` is
reserved for a bounded recovery or local-lab trigger. The driver reads the
authoritative SQLite state, takes the one legal next action, persists its intent
before any external write, observes the result, and repeats. It does not accept
an action order from a CLI caller, issue body, web UI, or external response.

After admission, normal progression is automatic: Codex implementation,
verification, fresh review, push, PR creation, required-CI reconciliation,
repair when a required check fails, squash merge, Linear completion observation,
and cleanup. The driver stays alive and polls while CI, I-Fan approval, or
Linear completion is pending. A valid exact-head
I-Fan approval is evidence, not a controller command; once observed, it lets the
same driver continue to merge and cleanup.

The CLI driver has a bounded process lifetime (24 hours by default; a deliberate
`--max-runtime` may be set up to seven days). It exits without speculative
repair for terminal states, `awaiting_human_decision`, `manual_intervention`,
process cancellation, or expiry of that process lifetime. Human decision and
conflict resolution are structured operator work that can be followed by a
restart-safe `drive`; I-Fan alone grants the final GitHub approval. The
lower-level state-specific CLI commands are kept for incident recovery and
deliberate E2E fault injection, not routine operation.

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

SQLite schema version 4 persists the implementation and review models on each
run and the requested model on every Codex attempt. Migration deliberately
leaves these fields empty for pre-version-4 runs: that empty value is explicit
legacy evidence, not permission to claim the current policy was historically
used. Such runs fail closed before another Codex execution. Codex CLI 0.144.1
does not provide a verified stable effective-model field in the JSONL contract,
so the controller records the authoritative requested model and does not infer
an effective model from unstable telemetry.

The SQLite adapter uses `modernc.org/sqlite`. Its pure-Go implementation avoids a
CGO compiler/runtime dependency and keeps local and race-test execution
portable. The trade-off is a larger indirect dependency graph and binary than a
CGO-backed SQLite driver. The controller still has only one direct SQLite
dependency and does not shell out to the `sqlite3` CLI.

## Post-approval delivery

Schema version 5 extends the durable trial beyond `approval_ready` with explicit
`pushing_branch`, `branch_pushed`, `opening_pr`, `pr_open`,
`reconciling_reviews`, `repairing`, `awaiting_human_approval`, `merging`,
`cleaning`, and terminal states. No generic publishing state hides multiple
external operations.

Every external operation follows an intent/reconcile/observe pattern. The
controller commits an idempotency key and immutable intent before invoking Git
or GitHub, compares actual external state, and then saves the observed result.
A restart never treats an interrupted process as evidence of success or failure.

Push uses the persisted branch in an explicit
`refs/heads/<branch>:refs/heads/<branch>` refspec and never force-pushes. The
controller revalidates the owned worktree, origin, clean status, candidate HEAD,
exact-HEAD verification, and latest passing fresh review. A matching remote SHA
is idempotent. A different SHA fails closed unless it is the persisted head of
the same open controller-owned PR; a repair may then use an ordinary
fast-forward, `--force-with-lease` update to the new verified candidate. The
persisted PR head is advanced before the next read-only GitHub reconciliation.

If that owned-PR repair update halts in `manual_intervention`, the explicit
`recover-owned-push` operator action can return only that run to
`approval_ready`. It proves unchanged Linear source and retained open PR
ownership, but performs no external write. The next driver push repeats local
exact-HEAD validation, remote observation, and the fast-forward lease before
updating the branch.

One run owns at most one pull request. Adoption requires durable ownership
metadata plus matching head, base, candidate SHA, PR identity, and body digest.
A same-named branch or matching head/base alone is insufficient. PR bodies carry
summary, rationale, validation, fresh-review, out-of-scope, and Linear magic-word
evidence.

Required checks and human-review status use bounded polling attempts, intervals,
and an overall deadline under the delivery driver. Observations are pending,
pass, actionable failure, infrastructure failure, or timeout. Pending CI,
exact-head approval,
and Linear completion leave the driver waiting and polling; a timeout
or conflicting result becomes auditable fail-closed intervention rather than a
manual sequence of normal-state commands. Unknown events remain telemetry.
The ephemeral Sol review remains the independent implementation-review gate.

Required-CI failures are normalized to a controller-generated finding with a
stable check identity and body digest. Repair resumes only the persisted Terra
session with controller-normalized stdin. Every new HEAD invalidates all older
verification, review, push, check, and human-approval authorization.

Only verifiable I-Fan approval for the exact PR HEAD, passing checks, and the
latest passing internal review authorizes merge. The
controller never approves its own PR. Merge is squash-only and records the
pre-merge head, base SHA, result SHA, and timestamp after re-reading GitHub.

Cleanup is a separate restart-safe state. Each owned resource has its own intent
and result. Base branches, user-created resources, changed refs, dirty worktrees,
and resources owned elsewhere are rejected. Partial failures preserve evidence
and retry only unfinished resources. Linear completion is observed but not
forced; production Linear writes remain deferred. Once a completion observation
is valid, cleanup is automatically driven rather than requiring an operator to
invoke a cleanup command.

## Fixture-first dogfooding

The deterministic integration suite is restricted to disposable repositories,
local bare origins, and fake GitHub services. The normal SQLite state and public
CLI are used across a simulated restart, and artifacts remain inspectable. A
separate external E2E run may use a credential-free GitHub origin binding only
for an explicitly authorized isolated test repository. This repository's
production remote is never a PR, merge, or cleanup fixture. See
[`docs/e2e-dogfood.md`](e2e-dogfood.md) for the operator-owned acceptance
matrix.

## Direct read-only GitHub App adapter

Schema version 6 added non-secret installation, repository, request, rate-limit,
actor-derived normalized evidence, and response digests. JWTs, installation
tokens, private keys, authorization headers, and raw token responses are never
persisted. The adapter uses RS256 App JWTs and memory-only installation tokens,
refreshes before expiry, and permits one refresh/retry after HTTP 401.

REST owns installation token minting, repository and pull-request identity,
check runs, and commit status evidence. GraphQL owns human-review identity
topology. Both transports are read-only, bounded, and
paginated. A final approval requires the configured User's immutable GitHub
identity and exact candidate head; display names and login similarity are
insufficient. Production use is explicit through `github-read` and does not
consult `gh` configuration or user credentials.
