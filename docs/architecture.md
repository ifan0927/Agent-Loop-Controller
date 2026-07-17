# Architecture

## 1. Design Goals

Agent Loop Controller is a deterministic state machine around nondeterministic
coding executors and external delivery systems. Its design optimizes for:

- exact evidence instead of agent assertions;
- one legal next action derived from persisted state;
- restart-safe and idempotent external effects;
- explicit human authority for ambiguous scope and final approval;
- isolated repository resources with provable ownership;
- narrow adapters rather than generic write clients;
- sanitized, durable observability without credential retention.

The controller does not implement code reasoning, a general workflow engine, or
an autonomous policy-improvement loop.

## 2. System Context

```text
Manual CLI / automatic worker / future Hermes or API adapter
                           |
                    admission signal
                           v
Linear task read -> immutable CodingTask + repository authority snapshot
                           |
                           v
             SQLite-backed production driver
               |           |             |
            Codex         Git          GitHub App
       implement/review  workspace   PR/checks/review/merge
               \           |             /
                exact-HEAD evidence gates
                           |
                           v
               Linear completion observation
                           |
                           v
              source sync and owned cleanup
```

Linear is the authoritative task source. Git and GitHub are authoritative for
repository and delivery facts. SQLite is authoritative for controller intent,
state, ownership, and recorded observations. I-Fan is authoritative for
structured task decisions, review-thread resolution, and final GitHub approval.
Codex output is input to validation; it is never authority by itself.

## 3. Component Responsibilities

| Layer | Responsibility | Must not know or own |
| --- | --- | --- |
| `internal/domain` | Pure contracts, state topology, evidence semantics, and validation | CLI, SQLite, HTTP, filesystem, or process details |
| `internal/application` | Use cases, authorization, orchestration, reconciliation, and ports | Flag parsing, concrete API clients, SQL, or shell execution |
| `internal/adapters` | SQLite, Git, Codex/process, Linear, GitHub App, configuration, verifier, and fixture implementations | Product policy beyond each typed port |
| `cmd/ifan-loop` | Composition root, CLI routing, flags, signal/time bounds, and JSON rendering | Alternate state transitions or duplicated domain policy |
| `contracts` | Versioned JSON schemas embedded into the binary for Codex outcomes | Workflow state or external side effects |

## 4. Trust and Authority Model

Authority is deliberately split:

| Decision or fact | Authority |
| --- | --- |
| Task goal, scope, criteria, priority, branch name | Current Linear issue, then immutable admitted snapshot |
| Repository, base branch, verifier IDs, GitHub App and trusted actors | Validated repository profile frozen into the run |
| Candidate content and ancestry | Managed Git observation |
| Test success | Latest complete verifier batch for the exact candidate HEAD |
| Internal review | Latest successful fresh review for that exact HEAD |
| CI and PR topology | Direct GitHub App observation for the owned PR and exact HEAD |
| Human review feedback | Trusted immutable I-Fan actor/review/comment/thread evidence |
| Final approval | Trusted GitHub review identity approving the exact current HEAD |
| Merge | Conditional GitHub result, or explicit evidence-gated external-merge acceptance |
| Controller progress and ownership | SQLite state, transitions, leases, intents, and evidence rows |

An authority record and prompt input are different. For example, trusted review
feedback is retained as immutable identity/body-digest lifecycle evidence; a
bounded normalized finding derived from it may be sent to Codex. The prompt
cannot replace the authority record.

## 5. End-to-End Data Flow

1. Admission reads Linear by identifier or scans a bounded eligible Todo set.
2. Eligibility requires team IFAN, current cycle, Todo, `agent:codex`, no
   `agent:hermes`, exactly one configured repository label, a safe Linear
   `branchName`, and parseable goal/acceptance sections.
3. The controller resolves verifier IDs and repository/GitHub authority from
   local configuration, then persists the immutable task and profile snapshots.
4. A dedicated worktree and artifact root are reserved before creation and
   checked against ownership markers on every resume.
5. Codex implements in the worktree. Structured semantic output is schema- and
   domain-validated; JSONL and stderr remain artifacts.
6. The controller commits the candidate, runs the configured verifier batch,
   and starts a new ephemeral read-only review against the exact branch delta.
7. Findings cause a bounded same-session repair followed by a new commit,
   verification, and fresh review. A pass reaches delivery authorization.
8. The production driver persists and reconciles branch push, owned PR,
   GitHub checks, trusted review feedback/reply, approval, and merge.
9. After GitHub merge, Linear is polled until it reports completed. The
   controller does not force completion.
10. A clean configured source checkout may fast-forward to the exact merge SHA;
    owned worktree and branches are cleaned independently and restart-safely.

## 6. Domain Model

### Task contract

`TriggerSignal` carries only source, issue ID, start action, requester, time, and
request ID. It cannot supply task contents. `CodingTask` freezes the normalized
issue identity, repository/base/working branch, goal, acceptance criteria,
out-of-scope items, controller-owned verifier IDs, source revision, and policy.
Validation protects safe Git branch syntax, non-empty criteria, verifier-ID
syntax, mandatory human approval, squash merge, and no silent scope expansion.

### State machine and legal transitions

`State` and `ValidateTransition` define the generic legal topology. Application
services add narrower evidence gates; being topologically legal is not enough.
For example, `fresh_review -> approval_ready` also requires a passing review,
successful verification, and matching current Git HEAD.

The topology protects against callers choosing arbitrary action order. Terminal
states have no outgoing generic edge. Only dedicated recovery services can use
the two narrow `manual_intervention` edges.

### Exact-head evidence

Candidate verification, internal review, pushed branch, PR head, required
checks, human approval, and merge precondition all carry a Git SHA. Any new
candidate invalidates authorization from the prior head. Evidence paths also
carry hashes and sizes so a modified artifact cannot silently retain authority.

### Verification, review, and approval authority

Verifier commands come from the controller-owned `builtin:v1` registry; Linear
may name only configured IDs. A verifier records whether the process was not
started, exited, or was interrupted, plus all output bindings. Review is a new
ephemeral Codex session in a read-only sandbox, never an implementation resume.
A human approval must come from the configured immutable GitHub `User` identity,
the owned PR, and the exact candidate reviewed internally and passed by CI.

### Human decision

A Codex `needs_human_decision` outcome supplies a bounded question and offered
choice IDs. `awaiting_human_decision` can continue only with a selected offered
choice stored alongside the originating outcome hash. The decision becomes an
authoritative contract clarification for same-session implementation and later
fresh review; it does not mutate the original task snapshot.

### Trusted review feedback lifecycle

Only an exact-head root inline `CHANGES_REQUESTED` comment by the configured
trusted I-Fan identity can enter the lifecycle:

```text
observed -> selected_for_repair -> repair_verified
         -> reply_pending -> replied -> resolved
                         \-> superseded (when authority becomes obsolete)
```

Immutable PR, review, thread, comment, actor, original-head, location, body
digest, and timestamps prevent a similar-looking comment from being adopted.
The controller may post one fixed marker-bound reply after repair verification
and fresh review. It never resolves the conversation.

### Reconciliation classification

GitHub check/review snapshots classify as `pending`, `pass`,
`actionable_failure`, `infrastructure_failure`, or `timeout`. Pending evidence
is polled; actionable failures may become normalized repair findings;
infrastructure or authority conflicts fail closed. Unknown external events are
retained as telemetry rather than becoming implicit success or a fatal parser
assumption.

### Cleanup ownership

Every managed artifact root, worktree, local branch, and remote branch has a
durable ownership row and expected identity/SHA. Cleanup operates per resource,
records intent/result independently, and refuses base branches, dirty or changed
resources, user-owned paths, and ownership conflicts. Artifacts and audit
evidence remain unless a specific owned-resource policy says otherwise.

## 7. Workflow State Machine

### Normal automatic states

```text
received -> admitting -> provisioning -> executing -> verifying -> fresh_review
  -> approval_ready -> pushing_branch -> branch_pushed -> opening_pr -> pr_open
  -> reconciling_reviews -> awaiting_human_approval -> merging
  -> awaiting_linear_completion -> cleaning -> completed
```

`rejected` and `failed` are terminal alternatives during admission or execution.
`repairing` loops through `executing`/`verifying`/`fresh_review` and returns to
delivery only with a new authorized head. `replying_review_feedback` is the
idempotent GitHub reply step after a verified trusted-feedback repair.

### Polling and waiting states

- The admission worker's admission interval gates idle scans of Linear Todo
  authority. A durable retry schedule instead waits until its SQLite
  `next_eligible_at`; scheduler lease renewal observes SQLite ownership while a
  dispatch is active. Neither timer sets delivery cadence.
- Action-scoped reconciliation, reply, cleanup, and local-controller lease
  tickers also observe only SQLite lease ownership. Their intervals derive from
  fixed lease TTLs; they do not poll GitHub or Linear and are not delivery
  readiness cadence.
- The production driver's independent delivery interval gates GitHub rereads in
  `pr_open`, `reconciling_reviews`, `awaiting_human_approval`, and
  `awaiting_github_mergeability`. Those reads observe PR/head/base, required CI,
  stable CI snapshots, review threads, exact-head approval, protection, and
  mergeability authority.
- The same delivery interval gates Linear completion rereads in
  `awaiting_linear_completion`, every retryable unavailable production action,
  and the no-wait immediate-action guard. Linear remains completion authority;
  the guard is internal loop-safety authority and cannot manufacture progress.
- Every configured wait is positive and bounded. Context cancellation interrupts
  admission and delivery timers promptly. A pending CI snapshot continues
  polling rather than becoming a failure; its durable evidence and slow-CI
  attention threshold remain independent of polling cadence.

Every retryable `unavailable` result from a production action uses the same
fixed delivery interval before another attempt:

| Production action | Authority revalidated by each attempt |
| --- | --- |
| Continue local | Persisted run/requester/state plus local Git, worktree, Codex process, verifier, and artifact evidence |
| Reconcile GitHub | Owned PR/head/base, required CI stable reads, review topology, exact-head approval, protection, and mergeability |
| Reply to review feedback | Persisted feedback/reply intent plus fresh GitHub comment/reply authority |
| Push branch | Exact-head approval, local candidate, configured remote, and restart-safe push evidence |
| Create or adopt PR | Exact-head approval, branch/head/base ownership, persisted create intent, and GitHub PR identity |
| Merge PR | Exact-head verification/review/check/approval evidence, current GitHub protection, and conditional squash-merge intent |
| Reconcile Linear completion | Recorded merge binding and fresh Linear issue completion state |
| Cleanup and source sync | Recorded merge/completion, owned local/remote resources, exact source state, and cleanup evidence |

This retry cadence is loop scheduling, not new authority. Each action retains
its existing compare-and-swap, fresh-read, lease, and idempotency gates.

### Human decision states

- `awaiting_human_decision`: one persisted offered choice must be submitted.
- `awaiting_human_approval`: no CLI approval action exists; I-Fan acts in
  GitHub and the controller observes it.

### Manual intervention

`manual_intervention` is a durable fail-closed stop for authority drift,
integrity conflict, unsafe recovery, exhausted repair policy, ambiguous external
result, merge rejection, or partial cleanup that cannot be retried safely. It is
not a general operator override.

### Terminal states

- `completed`: merge, Linear completion, and required cleanup evidence are done.
- `rejected`: admission rejected the task before delivery.
- `failed`: terminal execution/admission failure or an explicit eligible
  graceful abandon. Cleanup residue remains separately visible and does not
  retain the singleton active-run slot.

### Narrow recovery edges

- `manual_intervention -> approval_ready` is available only through
  `recover-owned-push` after proving an existing owned open PR and safe
  fast-forward repair recovery.
- `manual_intervention -> awaiting_linear_completion` is available only through
  `accept-external-merge` after proving exact candidate evidence, trusted
  approval, remote containment, and tree equality.

## 8. Application Modules

### Linear admission

**Purpose**

Read, validate, normalize, snapshot, and revalidate a Linear task.

**Inputs**

Issue identifier, requester, Linear reader, repository resolver, and persisted
run authority.

**Outputs**

An immutable `CodingTask`, repository/profile binding, or a sanitized drift and
eligibility failure.

**Authoritative state/evidence**

Linear source revision plus task/profile/registry digests stored on `runs`.

**External side effects**

Manual admission is read-only. Reserved automatic admission performs the one
configured Todo-to-In-Progress mutation after persisting intent.

**Failure and recovery behavior**

Repeated identical admission resumes; material drift stops rather than
rewriting a snapshot. Ambiguous mutation responses reconcile against the
admission journal.

**Key invariants**

Linear cannot supply executable commands, repository authority, or a
controller-chosen branch name.

### Automatic admission and worker

**Purpose**

Select at most one eligible Todo and run or resume its production driver.

**Inputs**

Validated automation authority, bounded candidate scan, scheduler lease,
existing nonterminal runs, retry schedules, and fixed trusted requester.

**Outputs**

Sanitized queue decision, driven run result, retry wait/schedule, or local
operator-attention event.

**Authoritative state/evidence**

Singleton admission lease, journal, run state, deterministic priority/team
sequence/UUID selection evidence, and retry schedule.

**External side effects**

One Linear state mutation and the existing driver's narrow side effects.

**Failure and recovery behavior**

One nonterminal run prevents scanning and is resumed first. Only typed process
start/temporary-unavailable failures receive bounded durable retries. A
complete valid bounded scan selects by priority, numeric Linear sequence, then
immutable UUID, independent of response order. Incomplete or contradictory
scans, conflicts, and exhaustion stop for attention.
Human-decision and manual-intervention states are parked with transition-bound
attention; GitHub approval remains inside the production driver's bounded poll
loop. Every dispatch cycle releases its short scheduler lease before waiting.
An authenticated `retry` action is deliberately narrower than general recovery:
it accepts only a current `retry_budget_exhausted` attention whose retained
failure class is `process_start`, with matching failed-attempt or verifier
process evidence identified by an exact persisted record reference while the
run remains before GitHub delivery authority, or
`unavailable` at the pre-provision admission boundary
where a successful fresh Linear read rechecks the dependency. The controller revalidates
Linear source, run/repository/key/state, transition sequence, local ownership,
and resolved side-effect evidence before atomically changing that exact
schedule to typed `operator_retry` eligibility. The attempt count, retry limit,
deadlines, state, and prior evidence are not reset. After the journaled action,
the next worker cycle resumes the same run through the normal production driver
without a separate drive command. A repeated failure increments the retained
attempt and produces a new stable attention event instead of looping.

GitHub required-check startup is a durable poll, not a retry failure. The
controller binds the first absent/queued/in-progress observation to the exact
run, repository profile digest, PR, and candidate head. A profile-owned slow-CI
threshold (20 minutes by default) emits one restart-stable `ci_wait_slow`
attention event but does not stop polling. Passing/actionable checks, a new
candidate head, or leaving review reconciliation closes the matching wait.
Check topology may advance during one bounded read; the later complete snapshot
is accepted only while repository, PR, head/base, protection, pagination, and
review authority remain unchanged.

**Key invariants**

No preemption, timestamp/FIFO ordering, or more than one active run. The total
order is controller policy and never derives from issue prose.
Retry cannot answer a human decision, approve or merge a PR, abandon a run, or
adopt unrelated external state.

### Local controller

**Purpose**

Provision the worktree, invoke/resume Codex, commit candidates, run verification,
launch fresh review, normalize findings, and enforce repair deadlines.

**Inputs**

Persisted run/task/profile, Codex executor, verifier registry, Git workspace,
optional validated human decision or normalized findings.

**Outputs**

New state, candidate/evidence records, or a human/manual/terminal stop.

**Authoritative state/evidence**

Codex attempts, session ID, outcome hashes, worktree/base/head observations,
verification batches, reviews, decisions, and repair anchor/deadline.

**External side effects**

Creates owned local resources, runs child processes, edits only through Codex in
the assigned worktree, and creates controller-authored candidate commits.

**Failure and recovery behavior**

Started attempts are inspected after restart; a recoverable implementation uses
a new attempt with the explicit persisted session. Missing or conflicting
session/evidence fails closed.

**Key invariants**

Implementation sessions resume; reviews never resume. Review artifacts cannot
overlap the writable worktree. Branch/base/head are revalidated around process
boundaries.

### Production driver

**Purpose**

Continuously derive and execute one safe next action from current persisted
state.

**Inputs**

Run ID, persisted requester/repository/idempotency authority, bounded policy,
coordinator, and action-specific ports.

**Outputs**

A durable human/manual/terminal stop or continued polling.

**Authoritative state/evidence**

The run re-read after every action; no stale action result drives the next step.

**External side effects**

Only those exposed by typed push, PR, reply, merge, Linear read, sync, and
cleanup ports.

**Failure and recovery behavior**

Pending/unavailable results poll; process cancellation or maximum runtime exits
without changing authority. `controller drive` reconstructs the same driver.

**Key invariants**

No caller, issue text, UI, or external response supplies action order.

### Production coordinator

**Purpose**

Apply application gates around each driver action and revalidate Linear and
persisted authority before local or external work.

**Inputs**

Typed command with requester, run, repository, expected state, idempotency key,
and optional decision; narrow action port.

**Outputs**

Typed action/result and updated run projection.

**Authoritative state/evidence**

SQLite run/inspection plus fresh Linear, Git, and GitHub observations required
by the action.

**External side effects**

Delegates one bounded action after intent is durable.

**Failure and recovery behavior**

Conflicts are classified and persisted when safe; caller retries or the driver
reconciles rather than blindly repeating writes.

**Key invariants**

The coordinator cannot bypass the local exact-head validator or state CAS.

### Query and status projection

**Purpose**

Return requester-authorized, credential-safe run status and detailed evidence.

**Inputs**

Immutable requester identity and run ID.

**Outputs**

Run summary/detail, state timeline, attempts, exact-head evidence, side-effect
records, attention, and safe recovery authority.

**Authoritative state/evidence**

SQLite inspection joined from the run-scoped evidence tables.

**External side effects**

None.

**Failure and recovery behavior**

Unauthorized, unknown, or identity-drifted requests fail without opening
external credentials.

**Key invariants**

Raw issue/task bodies, private paths where unsafe, tokens, keys, headers, and
unsanitized transport payloads are not projected.

### Human decision handling

**Purpose**

Validate and persist one choice from a Codex decision request, then resume the
same implementation contract.

**Inputs**

Decision JSON, exact expected state, requester, idempotency key, and originating
outcome evidence.

**Outputs**

Immutable decision evidence and transition back to `executing`.

**Authoritative state/evidence**

Choice ID, instructions, decision-file digest, and originating outcome
path/hash.

**External side effects**

None until the local controller resumes Codex.

**Failure and recovery behavior**

Changed files, unoffered choices, stale state, or outcome conflicts fail closed;
the persisted valid decision can be reused after restart.

**Key invariants**

The controller never invents or auto-selects a human choice.

### Repair and fresh review

**Purpose**

Convert controller-normalized CI, trusted feedback, or fresh-review findings
into bounded same-session repair and require new exact-head evidence.

**Inputs**

Immutable source IDs/digests, bounded normalized findings, repair policy, and
the persisted Terra session.

**Outputs**

New candidate, verification, fresh review, or manual intervention at deadline.

**Authoritative state/evidence**

Finding set, original/bound repair head, attempt/session, repair anchor, and
fresh-review outcome.

**External side effects**

Codex edits and local Git commit; later driver actions publish separately.

**Failure and recovery behavior**

Cancellation is resumable before the durable deadline. Expiry or malformed
anchor records a bounded manual stop.

**Key invariants**

Findings are prompt input, not external-write authority; every repair requires a
new head and full verifier/review cycle.

### GitHub reconciliation and trusted feedback

**Purpose**

Observe the owned PR, checks, review topology, trusted approval, feedback, and
mergeability; persist typed evidence and select wait, repair, reply, or merge.

**Inputs**

Owned PR evidence, expected head, trusted actor profile, paginated REST/GraphQL
reads, and current run state.

**Outputs**

Poll classification, findings/feedback lifecycle, approval, mergeability wait,
or manual intervention.

**Authoritative state/evidence**

Immutable GitHub identities, request/response digests, PR/head/base, checks,
review/thread/comment topology, approval, and timestamps.

**External side effects**

Reads only; reply and merge are separate narrow coordinator operations.

**Failure and recovery behavior**

Pagination overflow, partial GraphQL results, actor/topology drift, or ownership
conflict fails closed. Unsupported trusted-review topology is retained as a
finite sanitized reason: the split shape where an inline root belongs to a
`COMMENTED` review while a separate trusted exact-head review requests changes
is distinct from the generic unsupported-topology fallback. Feedback authority
drift and immutable conflicts also retain separate finite reasons. These reason
fields never contain review bodies or actor-controlled prose. When a trusted
review failure and premature Linear completion are observed together, the more
specific trusted-review reason remains authoritative. Pending states remain
read-only polls.

**Key invariants**

Login similarity and prose never establish trust; identity and exact head do.

### Merge and Linear completion reconciliation

**Purpose**

Perform one protected squash merge and wait for Linear's external completion
automation, or explicitly accept a separately verified external merge.

**Inputs**

Exact candidate, current base, passing verifier/review/checks, trusted approval,
owned PR, and Linear source.

**Outputs**

Merge record, completion observations, and transition to cleanup.

**Authoritative state/evidence**

Merge intent/result, pre-merge head/base, merge SHA/time, and Linear state
observation bound to that merge.

**External side effects**

Conditional squash merge. Linear completion reconciliation is read-only.

**Failure and recovery behavior**

An ambiguous merge response is re-read. A manually merged owned PR enters
manual intervention and requires the typed external-merge acceptance proof.

**Key invariants**

No automatic branch-protection bypass, alternative merge method, or forced
Linear completion.

### Source synchronization and cleanup

**Purpose**

Advance a safe configured source checkout to the exact merge and remove only
proven controller-owned resources.

**Inputs**

Persisted merge, repository binding, cleanup records, source/worktree/branch
observations, and ownership nonces.

**Outputs**

Per-resource results, a durable operator-attention event when required, and
`completed`.

**Authoritative state/evidence**

Expected refs/SHAs, resource ownership rows, sync before/after/merge SHAs, and
cleanup intent/result.

**External side effects**

Safe fast-forward and deletion of eligible owned worktree/local/remote branch.

**Failure and recovery behavior**

Partial progress is persisted and only unfinished resources retry. Unsafe dirty
source state remains untouched and produces sanitized attention.

**Key invariants**

No stash, reset, rebase, checkout switch, force deletion, or user-resource
adoption.

## 9. Adapter Modules

### SQLite

`internal/adapters/sqlite` is the durable store and migration owner. It enforces
foreign keys, busy timeout, expected-state CAS, unique ownership/idempotency
constraints, leases, atomic evidence/transition handoffs, and sanitized
inspection. The current schema is version 28; migration history is code, not a
human workflow API.

### Git and worktrees

`internal/adapters/git` provisions and validates isolated worktrees, observes
branch/base/head/status, creates controller-authored commits, publishes explicit
refspecs, verifies accepted external merges, synchronizes source checkouts, and
cleans resources. Commands are argv-only through the managed process adapter;
there is no shell interpolation.

### Codex process

`internal/adapters/codex` builds versioned implementation, resume, and fresh
review commands; preflights required CLI flags/version; materializes embedded
schemas; captures JSONL and stderr separately; extracts session evidence; and
validates structured outcomes. Current policy requests `gpt-5.6-terra` for
implementation/resume and `gpt-5.6-sol` for every fresh review. Managed runs
ignore global user configuration. Every Codex preflight and execution process
is associated with a controller-locked lifecycle record in its attempt
directory. A controller-owned launch supervisor blocks on a private gate until
the parent persists that record, so parent death before release cannot execute
the requested target. The supervisor remains the authenticated group leader,
drains the trusted Codex target and other members of its process group before
reporting completion, and prevents a leaderless live group. This boundary does
not claim containment of a trusted executable that deliberately detaches into
another process group or session; adversarial executable isolation is outside
the local macOS MVP. Only the controller retains the separate lock descriptor; a
restart may claim its authenticated inode after the prior controller releases
it. This lets graceful abandon interrupt the exact surviving process group and
prove it exited without trusting a reusable PID alone.

Managed launches created inside a generated Go test binary add a separate
test-parent lifetime pipe after the durable launch gate. Abrupt loss of that
test runner closes the pipe, so the supervisor drains only its own process
group instead of becoming long-lived test residue. The adapter enables this
contract only from the linker's Go testing-runtime marker and removes its
internal marker from inherited environments. Production binaries do not create
the lifetime pipe; authenticated process adoption after a controller crash
remains unchanged.

### Linear

`internal/adapters/linear` validates the official (or loopback fixture) GraphQL
endpoint, reads issues and candidates with bounded pagination, observes source
revision and workflow identity, and exposes the one reserved state mutation.
Credentials are re-read from the exact configured file or legacy environment
source; no fallback occurs.

### GitHub App REST and GraphQL

`internal/adapters/githubapp` mints short-lived installation tokens in memory,
checks numeric repository/installation authority, and performs bounded typed
operations. REST handles repository/PR/check/status/reply/merge operations;
GraphQL handles human review and thread topology. Only the configured capability
switches enable PR create, review reply, and squash merge writes.

### Bootstrap and configuration

`internal/adapters/bootstrap` loads strict configuration versions 1-3,
canonicalizes and cross-checks repositories and GitHub profiles, validates path
isolation, derives stable digests, and produces a credential-safe readiness
projection. Version 3 is current; older versions are compatibility inputs, not
recommended templates.

### Filesystem and artifact handling

Application artifact helpers and the process/Git/Codex adapters require new
empty attempt directories, exclusive output leaves, non-overlap with the
worktree, canonical containment, and stored hashes/sizes. Artifact contents are
private evidence, not query output.

### LaunchAgent and worker supervision

The CLI embeds a fixed worker plist template and implements safe render/install,
static validation, bounded `launchctl` control, and sanitized results. launchd
supervises one logged-in user's worker process; SQLite leases and journals—not
launchd—remain workflow authority.

The normal worker has no wall-clock expiry. SIGINT/SIGTERM stops new cadence,
cancels the active production driver and child processes, joins lease renewal,
releases the scheduler lease with a bounded cleanup context, and closes SQLite
without changing the durable run into failure or abandonment. A restart
re-enters persisted recovery before scanning, so process supervision cannot
authorize duplicate admission. Per-operation timeouts remain independent of
process lifetime, and GitHub App installation tokens refresh from their own
expiry metadata. LaunchAgent configuration and binaries use restart-to-reload.
Private stdout/stderr leaves use a fixed startup truncation threshold; normal
cadence does not produce per-cycle log records.
Sanitized worker output reports `running`, `driving`, `parked`, or `stopping`;
the final stopping record also identifies the immediately previous state. A
private atomically replaced status snapshot next to the controller config makes
the current state observable without appending one log record per poll;
LaunchAgent status projects it only while launchd observes the worker running
and the snapshot's PID plus OS process-start identity match launchd's current
process, so restart races and PID reuse cannot adopt a previous worker's state.

### Operator-attention boundary

Application services publish immutable versioned attention events through a
narrow append-only port. CLI inspection and future presentation adapters use a
separate bounded query port. The envelope contains only typed event, state,
severity, reason, repository profile, scope, digests, timestamps, and permitted
presentation action IDs. An advertised `retry` or `abandon` action is metadata;
it is never authentication or permission to mutate controller state.

SQLite is the initial durable adapter. Same-key same-payload publication is an
idempotent replay and a conflicting payload fails closed. Schema 23 preserves
the former local-delivery digest and status as legacy database evidence while
removing transport lifecycle from the application event and safe inspection
projection. Migrated rows remain immutable schema-0 events with their original
payload digest; current presentation actions are not backfilled into their
identity. Publication failure cannot authorize or advance workflow state.
Polling, human approval waiting, and successful terminal states do not emit
operator-error events. Every delivery-loop coordinator action re-reads its
final durable state and publishes the transition-bound manual-intervention
event before returning; the production driver repeats that publication
idempotently at its stop boundary. Human-decision stops similarly publish only
the typed `decide` presentation action. A restarted automatic dispatcher
reconstructs either event from durable transition evidence and publishes the
same event key, so foreground and worker recovery cannot create duplicate
parked-outcome events. Missing or drifting transition evidence remains parked
behind a stable authority-conflict event. Lease timestamp changes do not alter
event identity.

An explicit operator recovery answer crosses a separate authenticated
application boundary. Before any later retry or abandon mutation, the
controller records an immutable intent bound to the run, repository, expected
state, run idempotency key, transition sequence, parked attention key, typed
reason/action, and the configured requester's immutable GitHub identity. Only
an action advertised by that exact current attention event is eligible. The
same idempotency key and payload return the existing lifecycle record after a
restart or subsequent state advance; payload drift fails closed. A run,
transition, and parked attention event can own only one validated answer, so
concurrent contradictory actions fail closed.

The operator-action lifecycle is monotonic: `validated` records authority
before mutation, `applied` binds the resulting state and transition sequence,
and `observed` records a typed terminal result. Timestamps and sanitized
payload/applied-evidence/outcome digests make incomplete or ambiguous outcomes
inspectable without storing command arguments, paths, prose, or secrets. This
journal is distinct from automatic state transitions and side effects, so notification
delivery or an automatic controller step cannot be mistaken for human
authorization. The operator journal supplies this shared persistence and
application composition foundation. Typed retry composes it with the automatic
schedule. Graceful abandon composes the same journal with guarded ownership
cleanup: it records intent, attempts only proven-safe resources, terminalizes
even when cleanup is retained or fails, and publishes residue attention without
an advertised workflow action. Successful cleanup is not repeated after a
restart; artifacts and authority evidence remain queryable. An attempt is first
persisted as `prepared`, which proves that no process launch was authorized.
Immediately before the first Codex preflight, the controller durably commits it
to `started`; only started attempts require OS-process stop proof. Started Codex
attempts carry an authenticated controller-owned process identity. A random
per-attempt key remains in SQLite and authenticates the process group, exact
kernel start identity, bound lock inode, and exact per-attempt launch roster.
Every roster entry is required during stop proof, so an older completed
preflight record cannot hide missing current execution evidence. The launch
supervisor cannot execute the target before this identity is durable. The
controller retains its lock on
a private open-file description never inherited by the child, so the child
cannot unlock or replace controller authority; after a controller crash, the
next controller claims the same authenticated inode before signaling.
After action intent is durable, caller cancellation no longer controls the
bounded terminalization context. Cleanup has a narrower deadline than the
terminal transition, so exhausting its budget becomes residue rather than
stranding the singleton slot. Crash replay repairs an action result from the
persisted terminal transition before returning idempotent success.
Cleanup begins only after the authenticated identity
proves exit; missing, corrupt, or mismatched stop evidence, an orphan launch
lock, or a leaderless live process group retains all mutable resources. Exact
kernel identity, the leader's current kernel process-group membership,
process-group existence, and lock authority are rechecked before every signal
and throughout bounded exit proof. Remote
branches without a freshly observed open, owned,
unmerged PR are likewise retained instead of being treated as safe deletion
candidates; the mutation-authorizing read occurs after local cleanup and
immediately before the guarded remote deletion and terminal CAS. Failure or
authority drift on that final read retains remote residue but does not strand
the already-partially-cleaned run outside the terminal abandonment state. A
persisted remote-deletion intent is replayable across the narrow delete/result
crash window: only after managed-child exit proof does exact ownership plus a
fresh observed ref absence close the journal, and
only an authenticated current unmerged PR status may tolerate the now-missing
head identity. Once terminal, cleanup replay requires the current persisted
cleanup-residue attention; otherwise it is a no-op and cannot probe or mutate
owned resources.

### Hermes integration boundary

Hermes has no runtime adapter today. Its planned role is an authenticated
conversation, trigger, notification, and status interface over the same typed
application use cases and sanitized projections. It must not execute shell
instructions, read Mac files directly, manufacture decisions/approval, or own
workflow state.

## 10. Persistence Model

SQLite stores current state in `runs` and append-oriented or lifecycle evidence
around it. The principal table groups are:

| Table group | Responsibility |
| --- | --- |
| `runs`, `transitions` | Current run snapshot, requester/profile/task authority, lease, candidate, and ordered state history |
| `attempts`, `verifications`, `reviews` | Codex sessions/process results and exact-head automated evidence |
| `owned_resources`, `cleanup_results` | Resource ownership and per-resource cleanup progress |
| `side_effects` | Persisted external intent, claim, attempt, and observed result |
| `pull_requests`, `poll_observations`, `review_findings` | Owned PR and normalized GitHub/CI reconciliation evidence |
| `github_installations`, `github_request_observations`, `github_read_evidence` | Direct App/repository authority and sanitized transport observations |
| `human_approval_observations`, `human_approvals` | Rejected/pending/accepted approval reads and final exact-head authority |
| `trusted_review_feedback`, conflict and reply tables | Immutable trusted feedback lifecycle, drift conflicts, and one reply proof |
| `merge_results` | Controller squash or explicitly accepted external merge evidence |
| Linear request/completion and Todo admission tables | Linear observations, singleton scheduler lease, reservation/mutation journal |
| `automatic_retry_schedules`, `operator_attention_outbox` | Restart-stable retry policy and immutable versioned operator-attention events; legacy delivery fields are storage-only evidence |
| `operator_actions` | Explicit authenticated recovery intent and its validated/applied/observed provenance, separate from automatic workflow evidence |

### Current state versus evidence

`runs.current_state` answers where the controller is now. Transitions and
evidence tables answer why it may be there and what exact observations support
the next action. Updating current state without its required evidence is not a
valid recovery.

### Intent versus observation

For an external write, `side_effects` or its specialized table records immutable
intent and idempotency before invocation. The response or a later read records
observation. A `started` or pending intent is not success; restart reconciles the
target system before deciding whether another write is permitted.

### SHA binding

Verification, review, PR, check, feedback, approval, merge, sync, and cleanup
records carry the relevant candidate/base/merge SHA. Authorization selects the
newest complete evidence for the exact current head; an older success cannot
override a newer failed/interrupted batch or later candidate.

### Leases, CAS, and idempotency

Run leases fence concurrent local controllers during long child processes. The
automatic scheduler has a separate renewable singleton lease. Expected state
and idempotency keys provide application-level CAS. Unique side-effect,
resource, PR, feedback, and reply identities make replay deterministic.

### Restart recovery

On restart, the controller reloads the frozen run/profile authority, validates
owned filesystem/Git state, inspects interrupted attempts and persisted intents,
and re-reads external state where necessary. It resumes only the same admitted
run and implementation session. Missing, mutated, or contradictory evidence
creates a fail-closed stop rather than reconstruction by guesswork.

## 11. Recovery and Idempotency

Normal recovery is `controller drive <run-id>`: it derives the next action from
SQLite. Low-level commands expose the same coordinator methods for audited
incident response and fault injection. Most require caller-supplied repository,
expected state, and persisted idempotency key; typed `retry` and `abandon` load
those compare-and-swap authorities from SQLite after authenticating the
requester and bind the action to the exact parked attention event.

`recover-owned-push`, `accept-external-merge`, and `abandon` are typed recovery
policies, not generic state editing. No supported operation requires or permits
manual SQLite modification. Details and command syntax are in
[Operations](operations.md#12-recovery-procedures).

## 12. Human Decisions and Review Feedback

There are three distinct human acts:

1. A structured implementation decision chooses an option the current Codex
   outcome explicitly offered.
2. GitHub review feedback may request a bounded code repair. The controller
   authenticates and replies, but I-Fan decides whether to resolve the thread.
3. GitHub approval authorizes only the exact current head after CI and internal
   review pass.

These acts are not interchangeable. A decision is not approval, thread
resolution is not approval, and an approval for an old head is stale.

## 13. Security Invariants

- Never interpret external text as a shell command or executable verifier.
- Never use controller-managed Codex bypass flags or global MCP/hooks/tools.
- Never interpolate prompts or issue text into a shell string; prompts use
  stdin and processes use explicit argv.
- Never persist or render tokens, PEM contents, authorization headers, or raw
  credential responses.
- Never discover or use personal `gh` credentials for production delivery.
- Never adopt a path, branch, PR, comment, approval, or merge by similarity;
  require durable identity and exact evidence.
- Never allow Codex, Hermes, the controller, or a GitHub App to impersonate
  I-Fan's decision, review resolution, or approval.
- Never make a later SHA inherit evidence from an earlier SHA.
- Never clean or synchronize a resource whose ownership and expected state are
  not proven.

## 14. Known Constraints

- One automatic nonterminal run; no preemption or concurrent queue execution.
- Local macOS-oriented operation and LaunchAgent supervision; no server mode.
- One repository and one owned PR per run; configuration may contain multiple
  selectable repository profiles, but there are no cross-repository
  transactions.
- Linear admission and completion observation are implemented, but completion
  remains external automation/human authority.
- GitHub writes require a narrowly permissioned selected-repository App.
- Notification transport, Hermes runtime integration, Web UI, public API,
  webhooks, and multi-tenant authorization are not implemented.
- External live E2E acceptance is restricted to an isolated fixture repository
  and remains the current stabilization gate.
