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
Current CLI / automatic worker / future local TUI or adapter
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
state, ownership, and recorded observations. The configured human operator is
authoritative for structured task decisions, review-thread resolution, and
final GitHub approval. Codex output is input to validation; it is never
authority by itself.

## 3. Component Responsibilities

| Layer | Responsibility | Must not know or own |
| --- | --- | --- |
| `internal/domain` | Pure contracts, state topology, evidence semantics, and validation | CLI, SQLite, HTTP, filesystem, or process details |
| `internal/application` | Use cases, authorization, orchestration, reconciliation, and ports | Flag parsing, concrete API clients, SQL, or shell execution |
| `internal/adapters` | SQLite, Git, Codex/process, Linear, GitHub App, configuration, verifier, and fixture implementations | Product policy beyond each typed port |
| `cmd/agentctl` | Canonical composition root, CLI routing, flags, signal/time bounds, launchd compatibility migration, and JSON rendering | Alternate state transitions or duplicated domain policy |
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
| Human review feedback | Configured trusted reviewer actor/review/comment/thread evidence |
| Final approval | Trusted GitHub review identity approving the exact current HEAD |
| Merge | Conditional GitHub result, or explicit evidence-gated external-merge acceptance |
| Controller progress and ownership | SQLite state, transitions, leases, intents, and evidence rows |

An authority record and prompt input are different. For example, trusted review
feedback is retained as immutable identity/body-digest lifecycle evidence; a
bounded normalized finding derived from it may be sent to Codex. The prompt
cannot replace the authority record.

### Application authorization scopes

Configuration version 5 defines one controller-level operator as the complete
immutable GitHub `User` tuple: login, database ID, node ID, and actor type. It
is distinct from the automatic-admission requester and from any future
presentation session. Domain and application packages contain no browser,
cookie, or transport-session concept.

Application authorization uses a closed set of authority scopes rather than
roles or a generic permission language:

- `controller` covers controller-wide readiness, configuration, supervisor,
  capacity, and audit projections and requires the exact configured operator;
- `repository` derives from the current repository profile authority and
  remains readable when a later lifecycle adapter disables admission;
- `run` derives from the repository authority frozen into that run, so mutable
  enablement or profile removal cannot rewrite historical run authority;
- `onboarding` derives from the configured operator before repository binding
  and from the resulting repository authority after binding.

Collection authorization follows one order:

```text
resolve configured requester
  -> derive authorized scopes
  -> filter persisted rows by scope
  -> count and order
  -> paginate
  -> sanitize and project
```

SQLite run and scheduling collection ports require an application-produced
scope set. Hidden rows cannot affect totals, page gaps, `has_more`, or cursor
position. Run cursors bind the query filter and current scope digest, so
authority drift requires a new first-page query. Repository/run scheduling
projections expose only their own wait/supervisor state; controller capacity and
sibling identities require controller scope.

Direct unknown and unauthorized run or repository targets return the same
sanitized application `not_found` result. Visibility of an attention event,
identifier, or cursor grants no mutation authority. Sensitive commands do not
accept scope snapshots as bearer grants: each command re-reads current
repository authority or the target run's frozen authority and preserves its
existing exact-head, CAS, lease, idempotency, and reconciliation checks. The
current CLI requester flags remain compatible by entering these same
application contracts.

### Legal actions and operation receipts

The Controller derives legal-action offers only from authorized persisted run,
transition, scheduling, recovery, and current-attention evidence. An offer has
an opaque stable identifier, typed action, sanitized reason, required
confirmation/input kind, and consequence. It does not expose expected or target
state, transition sequence, action/run idempotency keys, command arguments,
paths, sessions, or mutable workflow authority. Possessing an offer is not
authorization: execution resolves the configured immutable operator again,
loads only the encoded target, re-reads its frozen authority, and recomputes the
current offer before entering the existing action-specific service.

The six current actions are `decide`, `retry`, `abandon`, `recover_ci_wait`,
`recover_owned_push`, and `accept_external_merge`. Attention `allowed_actions`
and offered legal actions use the same eligibility predicates. Each typed
executor retains its own revalidation and safety gates; there is no generic
state-mutation endpoint.

Before the first controller mutation, every accepted action is atomically bound
to a common scope-neutral receipt. Controller, repository, run, and onboarding
targets share the same envelope without invented run fields. Deterministic
operation identity combines the exact requester, target scope, target,
authority digest, operation type, and request digest. A private
Controller-derived operation anchor binds the exact attention/transition or
equivalent future mutation occurrence; it is neither rendered nor accepted as
presentation authority. Exact replay returns the persisted receipt. Within one
anchor, payload, action, or expected-authority drift conflicts, while a later
Controller-owned occurrence receives a new anchor. Mutually exclusive actions
share the same authority uniqueness boundary.

Configuration same-digest no-ops are not a weaker receipt path. SQLite
rechecks the exact desired generation, digest, and absence of an incomplete
intent, then writes the final observed/succeeded receipt in that same
transaction. A concurrent authority advance conflicts instead of recording a
receipt against stale authority.

Receipt phase and outcome are separate. The monotonic phases are `accepted`,
`applied`, and `observed`; outcomes are `pending`, `succeeded`, `failed`,
`conflict`, and `ambiguous`. An interrupted caller cannot erase an accepted
receipt. Restart reconciliation uses persisted action and transition evidence
to complete or classify it without repeating a proven mutation. The authorized
single-receipt query returns the same sanitized `not_found` result for missing
and unauthorized targets and reads only persisted Controller state.

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

The `IFAN` Linear team key and `IFAN-*` issue identifiers remain exact external
runtime compatibility contracts, and `github.com/ifan0927` remains the current
repository/module identity. They do not define generic product or operator
terminology. New local runtime identities are `agentctl`,
`io.agent-loop-controller.worker`, the neutral managed-launch protocol, and the
neutral review-reply marker. The process adapter and GitHub reader still accept
the corresponding legacy markers where authenticated in-flight process or
external idempotency evidence may outlive installation migration; new records
never write the personalized forms.

| Retained legacy identity | Classification and removal boundary |
| --- | --- |
| `ifan-loop` executable path | Migration detection and rollback input only; no alias is built or installed. The exact old binary may be removed after the documented restart/adoption and rollback-retention gates. |
| `com.ifan.agent-loop-controller.worker` | Installed/loaded legacy supervisor detection and reversible plist restore only. New templates and services use the neutral label. |
| `--ifan-loop-managed-launch` / `IFAN_LOOP_INTERNAL_MANAGED_LAUNCH` | Required compatibility read for an already-started authenticated managed helper. New helpers write only the `agentctl` protocol. |
| `ifan-loop-review-reply:v1:` | Required compatibility read for existing GitHub reply idempotency evidence. New replies write only `agentctl-review-reply:v1:`. |
| `secret://env/IFAN_LOOP_LINEAR_TOKEN` and scrubbed `IFAN_LOOP_*` secret names | Existing explicit configuration compatibility and deny-list protection. New configuration defaults to the neutral file credential source; legacy names are never injected into managed children. |
| `I-Fan`, `IFAN`, `IFAN-*`, and `ifan0927` | External Linear/GitHub identity or historical fixture evidence, not local product naming. |

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
A post-repair review receives the exact persisted finding set that authorized
the repair, the previous candidate SHA, and the repaired candidate SHA. Finding
text remains explicitly untrusted and subordinate to the frozen task. The
reviewer must inspect both the repair delta and the complete branch delta, and
the versioned outcome must cover every expected `source`, `source_id`, and body
digest exactly once. A pass requires every expected finding to be materially
addressed and permits no new code findings. Missing, duplicate, stale, or
non-addressed dispositions fail closed; initial reviews require an empty
disposition set. Legacy outcomes may be replayed only when no repair-finding
context is required.
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
trusted reviewer identity can enter the lifecycle:

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
- `awaiting_human_approval`: no CLI approval action exists; the configured
  human reviewer acts in GitHub and the controller observes it.

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
  retain a repository scheduling slot or heavy-work permit.

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

Resume existing work and admit eligible Todos within persisted repository and
local-heavy-work capacity authorities.

**Inputs**

Validated automation authority, bounded candidate scan, short admission lease,
repository slots, per-run execution leases, heavy-work permits, retry schedules,
and fixed trusted requester.

**Outputs**

Sanitized queue decision, driven run result, retry wait/schedule, or local
operator-attention event.

**Authoritative state/evidence**

Short global admission lease, reservation journal, immutable repository slot,
per-run execution lease, heavy-work permit, run state, latest complete queue
snapshot, append-only scheduling decisions, and retry schedule.

**External side effects**

One Linear state mutation per admitted task and each run's existing narrow
driver side effects.

**Failure and recovery behavior**

Restart reconciliation enumerates every nonterminal run, validates immutable
repository bindings, quarantines duplicate repository ownership, reconstructs
supervision, and adopts or safely settles persisted scheduling authorities
before new admission. Existing runnable work is ordered by durable
`runnable_since` then run ID. New admission ranks Linear priorities 1 through 4,
then unprioritized work, with numeric IFAN sequence and immutable UUID as
tie-breakers. An active repository candidate is classified and skipped so an
idle repository may proceed; a typed disabled repository is likewise skipped.
The authoritative top-candidate reread may skip only definitive invalidity.
Ranking/source drift requires a fresh scan, while repeated ambiguous reads stop
for attention without admitting lower-ranked work.

Admission atomically reserves the run, repository slot, initial heavy permit,
and decision evidence before the Linear mutation. The global admission lease is
released before local execution. Local Codex, verifier, fresh review, and repair
states consume a heavy permit. External CI/Linear waits, human waits, manual
intervention, retry delay, and terminal states release it only after managed-
child stop is proven. A permit heartbeat or lease expiry is never process-stop
evidence. Lowering capacity enters drain mode and never cancels an existing
permit holder. External polls advance durable `runnable_since` by the configured
delivery interval, and the worker wakes at the earliest persisted runnable
deadline instead of the slower admission-scan cadence; human waits are not
runnable. Manual continuation crosses the same permit boundary as automatic
delivery. A supervisor may adopt a differently owned permit only while holding
the database-directory process lock, so lease gaps are not treated as proof
that the prior supervisor exited. The worker retains one
admission/poll supervisor beyond heavy capacity so waiting runs cannot starve an
idle repository.

Worker runtime liveness is a separate application contract from workload
activity and configuration convergence. After strict configuration, process
lock, credential topology, SQLite compatibility, supervisor fencing,
scheduling reconciliation, and production dispatcher construction succeed,
the active supervisor publishes schema-v2 private heartbeat evidence
immediately and on a fixed 15-second cadence. This is normally the automatic
worker; the mutually exclusive manual `linear start` and `controller run`
supervisors publish the same process-bound evidence for their own lifetime.
Activity transitions may publish immediately but do
not reset that cadence. The single supervisor heartbeat covers all bounded
repository dispatches; there is no per-run heartbeat. Publication failure
cancels and joins dispatch and fails the worker nonzero, while SQLite workflow
state and ordinary restart reconciliation remain recovery authority.

The heartbeat atomically replaces the prior private telemetry leaf and binds
the worker instance, PID, OS process-start identity, binary build identity,
exact loaded configuration digest, sanitized activity, cycle metadata, and
observation time. It is not an append-only audit stream. Schema-v1
activity-driven snapshots are finite legacy inputs and never current heartbeat
evidence. The controller-authorized runtime observation service uses injected
time plus a narrow process-identity observer to return `fresh`, `stale`,
`offline`, `unknown`, or `conflict`; only age at or below 45 seconds with an
exact live process match is fresh. Its sanitized projection omits PID,
process-start identity, UID, local paths, launchd output, raw errors, logs, and
credentials. Loaded digest evidence deliberately has no generation ID. The
configuration convergence service correlates only a fresh, identity-verified
loaded digest to the current desired generation under SQLite CAS; the worker
never invents generation identity.

Controller-owned configuration authority is established on the first
production configuration/store composition. The bootstrap adapter validates
one exact bounded byte payload, the private configuration adapter retains those
same bytes and exclusively publishes a filesystem baseline-binding intent,
SQLite durably prepares the matching database anchor, and only then may the
private locator be published. Startup proves a locator or pre-locator binding
target in read-only mode against that prepared binding before opening or
migrating the database. That proof accepts only the configuration-authority
schema floor through the binary's supported schema, allowing a trusted older
store to receive normal forward migrations while rejecting pre-authority and
newer unsupported stores. SQLite then atomically assigns one baseline generation without
rewriting the live file. The mode-`0600` locator beside the configuration binds
the canonical live path to its owning database so a later invalid or
database-path-drifted file cannot create, redirect, or migrate an attacker-
selected store. Raw
generation payloads remain mode-`0600` beneath a current-user mode-`0700`
authority directory; SQLite contains metadata, receipts, and sanitized events
only. Current plus nine recent settled payloads are retained, while current and
unresolved evidence are never pruned. Deletion first acquires a durable digest
claim in SQLite; apply acceptance checks that claim in its transaction, so a
digest cannot become accepted while its raw leaf is being removed. Startup
finishes interrupted claims idempotently.

The presentation-independent apply service authorizes from the committed
desired generation's configured operator, strictly validates current-schema
candidate bytes, compares expected generation and digest, protects every
nonterminal run's frozen repository and configured-operator authority, retains
the target, and commits one apply intent and operation receipt before a same-
directory atomic exchange. The exchange captures the exact displaced inode;
an unexpected concurrent edit is restored instead of overwritten. The
captured parent remains as private operation evidence until directory sync and
exact reread prove the target. Removing the staged leaf also requires a proven
parent-directory sync; an interrupted or failed cleanup remains an error until
reconciliation repeats that sync. Startup and pre-apply reconciliation settle an
interrupted intent from exact parent/target evidence or an ambiguous third or
unsafe observation; drift is never adopted or overwritten. A matching fresh
heartbeat durably selects only the current desired generation as effective.
The resulting finite projection is
`ready`, `restart_required`, `starting`, `stale`, `offline`, `unknown`, or
`conflict`, separate from worker activity and capacity.

Every production new-admission path uses this application-owned convergence
gate. Automatic dispatch may continue driving an already-admitted run, but it
checks the gate before candidate scan or Linear mutation and again immediately
before reservation. Manual Linear admission checks before issue collection and
again immediately before run creation. The second decision carries a
generation/digest/authority-version token that SQLite validates in the same
transaction as the new run or reservation. The token expires with the exact
heartbeat freshness window, and every durable drift-entered or drift-cleared
transition advances authority version and invalidates older tokens. Apply
acceptance uses the same SQLite write authority, so
admission cannot cross an accepted configuration change. A schema-31-or-newer
store with no configuration authority rejects direct run creation and automatic
reservation as well as composed admission. Missing authority,
pending restart, drift, unresolved apply,
stale/offline runtime, and unavailable evidence fail closed without releasing
permits or changing existing runs.

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

No preemption, weighted fairness, aging, or more than one nonterminal run for an
immutable repository binding. Heavy capacity is a generic positive integer;
the default is two and a legacy version-3 configuration maps to one. The total
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
records, attention, and safe recovery authority. `status` and `inspect` use the
same version 2 detail projection contract:

- `pull_request_aggregate` is explicitly labelled as the mutable controller
  aggregate used by delivery commands. It is not historical evidence.
- `pull_request_observations` contains immutable creation-journal and GitHub
  read observations in deterministic persisted order. GitHub history is a
  bounded latest-100 window; `pull_request_observations_total` and
  `pull_request_observations_truncated` disclose the durable total and whether
  older rows were omitted. These retain queryable historical PR facts without
  rewriting the aggregate.
- `pull_request` is the effective status. A valid immutable `merge_result`
  projects `merged` even when the aggregate remains open. Missing aggregate or
  incomplete persisted repository binding or terminal merge authority projects
  `unknown`; an aggregate must have complete PR identity and match the run's
  branch, base branch, candidate head, base SHA, and ownership key before it can
  support that result. Mismatched repository/PR/topology/merge evidence projects
  `conflict`. Merge authority requires full lowercase hexadecimal pre-merge,
  base, and merge commit SHAs. The complete retained GitHub-read history is
  checked before projection; any read without an observation timestamp projects
  `conflict` and cannot be hidden by a later valid read.
- each trusted feedback item labels its initial change-request snapshot,
  exposes controller lifecycle fields separately, and derives
  `effective_thread_status` from the latest repository-, PR-, and strict
  thread-topology-matching immutable GitHub read across the retained evidence
  history, or from a controller-recorded resolution observation. Missing or
  authority-conflicting evidence projects `unknown` or `conflict`. An
  unresolved GitHub read earlier than a controller resolution remains history;
  an equal-time or later unresolved read conflicts because neither source
  establishes a safe final resolved state at that ordering boundary. Both
  GitHub-backed and controller-backed thread status require a valid run-bound
  PR aggregate and a persisted feedback row with complete immutable identity,
  a legal lifecycle/evidence combination, and ordered nonzero timestamps.

**Authoritative state/evidence**

SQLite inspection joined from the run-scoped evidence tables. Effective
terminal facts are derived only from typed merge results, trusted feedback
lifecycle evidence, and sanitized GitHub read evidence; transition prose and
agent claims are not projection authority. SQLite verifies each GitHub evidence
digest and requires its SQL head SHA, repository ID, and canonical UTC
observation time to match the digest-bound JSON. Equivalent legacy JSON timezone
offsets remain valid, and equal instants are ordered by persisted evidence ID.
Every durable row is audited even when it falls outside the output window.
Inspection retains only that window, the global latest row, and one
application-selected candidate per feedback root, so memory is bounded while an
older matching thread remains effective behind arbitrarily many unrelated
polls.

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
inspection. The current schema is version 31; migration history is code, not a
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

`internal/adapters/bootstrap` loads strict configuration versions 1-5,
canonicalizes and cross-checks repositories and GitHub profiles, validates path
isolation, derives stable digests, and produces a credential-safe readiness
projection. Version 5 is current; older versions are compatibility inputs, not
recommended templates. For a legacy baseline it derives a migration-only
controller operator only when the same immutable actor is already trusted by
every repository profile. A disjoint legacy policy remains readable and can
converge through the Controller's internal admission gate, but it supplies no
controller-scope apply/history authority; a repository-only actor is never
elevated. Existing legacy run/query authorization is unchanged. Its same-byte
validation seam is reused by baseline,
live reread, retained desired evidence, and current-schema apply validation.
`internal/adapters/configuration` owns the baseline-binding intent, trusted
locator, immutable raw generation files, retention, captured-parent exchange
evidence, and atomic live replacement; it does not expose a raw history API.

### Filesystem and artifact handling

Application artifact helpers and the process/Git/Codex adapters require new
empty attempt directories, exclusive output leaves, non-overlap with the
worktree, canonical containment, and stored hashes/sizes. Artifact contents are
private evidence, not query output.

### launchd and worker supervision

The CLI embeds separate exact LaunchAgent and LaunchDaemon plist templates and
implements safe render/install, static validation, bounded `launchctl` control,
and sanitized results. Both templates use the neutral
`io.agent-loop-controller.worker` label and canonical `agentctl controller
worker` argv. The LaunchAgent supervises one logged-in user's worker;
the system LaunchDaemon supports pre-login headless recovery but pins
`UserName`, `HOME`, and `WorkingDirectory` so the worker remains the configured
non-root user. The system plist is root-owned; configuration, credentials,
database, artifacts, status, and logs remain owned by the worker user. Secrets
never enter a plist.

Only `gui/<uid>` and `system` are supported supervisor domains. A user-domain
service is not a substitute for either contract. Install, bootstrap, kickstart,
and status inspect both neutral and legacy labels in the selected and opposite
domains, including stopped installed plists, and fail closed on unverified or
dual topology. Independently, the
worker acquires a private advisory lock for its complete process lifetime before
constructing the scheduler runtime; a second LaunchAgent, LaunchDaemon, or
manual worker therefore exits before it can scan or recover work. This process
fence complements the short global admission lease. Manual `controller
continue` and experimental `local continue` acquire the same lock before they
authorize permit adoption, and an interrupted managed attempt must be stopped
and reconciled before its permit changes owner. SQLite authorities and
journals—not launchd or the process lock—remain workflow authority.

The explicit launchd identity migration first observes all four label/domain
combinations, boots out only the selected legacy service, proves exact-label
absence, and then takes the same worker lock before changing plist identities.
The legacy plist is retained under the non-loadable
`.agentctl-rollback` suffix; an interrupted migration is resumed from that
artifact. Rollback proves neutral absence under the same fence, preserves the
neutral plist under `.agentctl-disabled`, restores the legacy plist, and only
then bootstraps the legacy label. Atomic hard-link-and-unlink moves make an
interrupted file transition detectable without overwriting an unknown path.
Neither direction opens or rewrites SQLite, configuration, credentials,
artifacts, or run evidence.

The normal worker has no wall-clock expiry. SIGINT/SIGTERM stops new cadence,
cancels active production drivers and child processes, releases the admission
lease with a bounded cleanup context, and closes SQLite
without changing the durable run into failure or abandonment. A restart
re-enters persisted recovery before scanning, so process supervision cannot
authorize duplicate admission. Per-operation timeouts remain independent of
process lifetime, and GitHub App installation tokens refresh from their own
expiry metadata. LaunchAgent and LaunchDaemon configuration and binaries use
restart-to-reload.
Private stdout/stderr leaves use a fixed startup truncation threshold; normal
cadence does not produce per-cycle log records.
Sanitized worker output reports `running`, `driving`, `parked`, or `stopping`;
the final stopping record also identifies the immediately previous state. A
private atomically replaced heartbeat next to the controller config makes the
current runtime observable without appending one log or database record per
cadence. LaunchAgent and LaunchDaemon status enter the same controller-scope
runtime observation contract, verify the heartbeat PID and OS process-start
identity, and require the heartbeat PID to equal launchd's current process.
Restart races, stale files, legacy activity snapshots, and PID reuse therefore
cannot be adopted as a fresh worker. Privileged launchd mutation remains in the
CLI/runbook boundary; runtime observation adds no start, stop, install,
restart, or migration authority.

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

`operator_actions` remains the action-specific compatibility journal and is
atomically mirrored into the common `operation_receipts` contract. The legacy
operator-action lifecycle is monotonic: `validated` records authority
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
stranding the repository slot. Crash replay repairs an action result from the
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

### Planned local operator interface boundary

The planned routine local operator interface is a TUI over Controller-owned
application contracts:

```text
current CLI ----\
                 +--> Controller application services / projections
planned TUI ----/                 |
future HTTP ----------------------/
                                  v
                         domain + persistence
                                  |
                                  v
                           external adapters
```

Controller application services define authorization, legal-action offers,
operation receipts, idempotency, and audit evidence independently of
presentation; repository onboarding and configuration transactions remain
later foundations. CLI, TUI, and any future HTTP adapter call the same typed
services. A presentation adapter may
authenticate, validate, bound, and render requests; it may not duplicate state
transitions, authorization, scheduling, or external-write policy.

The initial TUI belongs in this repository, Go module, and product binary. Its
canonical planned entrypoint is `agentctl operator`; the worker's planned
canonical entrypoint is `agentctl controller worker`. They run as separate
processes. The TUI does not start, stop, own, or supervise the worker, and its
exit cannot stop Controller execution. Both processes rely on durable Controller
state plus Controller-owned authorization, CAS, leases, idempotency, receipts,
and reconciliation rather than shared in-memory authority.

The TUI never accesses SQLite, Controller configuration, credentials, GitHub,
Linear, worktrees, artifacts, raw logs, or arbitrary local paths to assemble
workflow policy. It consumes bounded sanitized projections and only the legal
actions offered by Controller application services. It is not a database
administration tool, orchestrator, or second state machine.

Repository onboarding remains a persisted Controller saga. A new-project flow
begins from an existing empty GitHub repository, creates the managed local
checkout and initial base revision, creates or adopts the exact Linear
`repo:<slug>` label, validates the repository profile, applies configuration,
and observes worker readiness. An existing-project flow validates and adopts a
matching local checkout and GitHub origin without rewriting user-owned Git
state. Partial progress is resumed or reconciled; external resources are not
destructively rolled back by implication.

GitHub repository creation, project templates, UI secret provisioning, GitHub
approval or review resolution, general Linear issue editing, privileged helper
installation, and break-glass recovery remain outside the TUI. It has no
notification inbox. HTTP, a Web UI, Hermes, and future outbound channels remain
optional adapters introduced only by demonstrated consumers; they are not TUI
prerequisites and never gain workflow authority.

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
| Linear request/completion and Todo admission tables | Linear observations, short admission lease, reservation/mutation journal |
| `heavy_capacity_authority`, `repository_slots`, `heavy_permits`, `run_scheduling` | Effective capacity, per-repository exclusion, local-heavy-work ownership, and durable runnable order |
| `queue_snapshot`, `scheduling_decisions` | Bounded replaceable queue projection and append-only authority-changing decision evidence |
| `automatic_retry_schedules`, `operator_attention_outbox` | Restart-stable retry policy and immutable versioned operator-attention events; legacy delivery fields are storage-only evidence |
| `operator_actions` | Action-specific authenticated recovery intent and legacy validated/applied/observed provenance, separate from automatic workflow evidence |
| `operation_receipts` | Scope-neutral accepted/applied/observed operation identity, outcome, and sanitized result evidence; legacy operator actions are backfilled and mirrored here |
| configuration generation, authority, apply-intent, and convergence tables | Immutable desired/effective metadata, one Controller-wide CAS transaction, reconciliation state, and meaningful sanitized transitions; raw bytes remain outside SQLite |

### Current state versus evidence

`runs.current_state` answers where the controller is now. Transitions and
evidence tables answer why it may be there and what exact observations support
the next action. Updating current state without its required evidence is not a
valid recovery. Query projections also distinguish stored observations from
effective facts: the mutable PR aggregate supports workflow commands, immutable
creation/read observations retain what was reported at each point in time, and
current terminal status is derived from later typed merge or thread-resolution
evidence without updating those earlier observations.

Transport-neutral scheduling read ports expose bounded active-run scheduling
state and recent decision detail without running reconciliation or triggering
external observation. Capacity and latest-queue reads are likewise read-only;
restart reconciliation remains a separate authority-changing operation.

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

Run leases fence concurrent local controllers during long child processes. A
short global lease serializes only admission decisions; repository slots fence
nonterminal work per immutable binding, and heavy permits fence generic local
execution capacity. Each authority has a separate version for CAS. Expected
state and idempotency keys provide application-level CAS. Unique side-effect,
resource, PR, feedback, and reply identities make replay deterministic.

### Restart recovery

On restart, the controller enumerates all nonterminal runs before admission,
reloads frozen run/profile authority, validates repository-slot uniqueness,
reconciles permits and execution leases with managed-process evidence, and
re-reads external state where necessary. It resumes only the same admitted run
and implementation session. Missing, mutated, globally ambiguous, or
contradictory evidence creates a fail-closed fence rather than reconstruction by
guesswork.

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
   authenticates and replies, but the human reviewer decides whether to resolve
   the thread.
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
- Never allow Codex, Hermes, the controller, or a GitHub App to impersonate the
  configured human operator's decision, review resolution, or approval.
- Never make a later SHA inherit evidence from an earlier SHA.
- Never clean or synchronize a resource whose ownership and expected state are
  not proven.

## 14. Known Constraints

- One automatic nonterminal run per immutable repository binding, bounded by
  configured local-heavy-work capacity; no preemption or cross-run authority.
- Local macOS-oriented operation with GUI LaunchAgent or headless system
  LaunchDaemon supervision; no remote controller service or multi-host mode.
- One repository and one owned PR per run; configuration may contain multiple
  selectable repository profiles, but there are no cross-repository
  transactions.
- Linear admission and completion observation are implemented, but completion
  remains external automation/human authority.
- GitHub writes require a narrowly permissioned selected-repository App.
- Typed drafts/semantic preview/rollback and drift recovery, transactional
  repository onboarding, the local TUI, optional HTTP/Web adapters,
  notification transport, Hermes runtime integration, public API, webhooks,
  and multi-tenant authorization are not implemented.
- External live E2E acceptance remains restricted to isolated fixture
  repositories. The automatic-delivery acceptance and repair-aware independent
  review are complete; future live gates must follow the staged Controller and
  operator-interface contracts without broadening production authority.
