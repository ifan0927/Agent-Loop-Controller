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

### Product runtime and repository development

ALC can manage Codex executions for configured target-repository workloads.
Development of the ALC repository itself is deliberately outside that runtime:
work is captured in a GitHub issue and the user manually launches Codex to
design or implement it. ALC never invokes, supervises, verifies, approves,
publishes, or merges changes into itself. This prevents a self-hosted or
recursive development authority.

Issue Spec Studio may guide a manually launched ALC design session when the
root router selects design mode. `ISSUE_SPEC_STUDIO_PATH`, the Project Profile,
and an optional active design checkpoint are development context only. They are
not Controller configuration, process inputs, prompt payloads, scheduler stages,
verification stages, or automatically loaded runtime context. Production ALC
does not require or read the variable and behaves identically when it is unset.

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
receipt against stale authority. Once recorded, exact replay resolves the
receipt's permanent resulting generation even after desired authority advances.

Receipt phase and outcome are separate. The monotonic phases are `accepted`,
`applied`, and `observed`; outcomes are `pending`, `succeeded`, `failed`,
`conflict`, and `ambiguous`. An interrupted caller cannot erase an accepted
receipt. Restart reconciliation uses persisted action and transition evidence
to complete or classify it without repeating a proven mutation. The authorized
single-receipt query returns the same sanitized `not_found` result for missing
and unauthorized targets and reads only persisted Controller state.

### Repository lifecycle and readiness

Every configured repository has Controller-owned lifecycle intent independent
of profile configuration: `enabled` or `disabled`. A lifecycle row represents
one immutable repository incarnation rather than the reusable canonical name.
The first managed open adopts exactly one durable baseline and preserves
configured repositories as enabled. It does not infer operational readiness
from historical runs, so new admission remains fenced; each initial snapshot
exposes all dimensions as `unknown` with `initial_recheck_required`.

A recheck is a receipt-backed read-only observation operation. It records the
exact profile, lifecycle version, configuration generation/digest/version, and
one result for each closed dimension: profile configuration, configuration
convergence, local checkout, base branch, GitHub repository, GitHub App, Linear
label, and verifier policy. Statuses are `ready`, `not_ready`, `unknown`,
`conflict`, and `not_applicable`; aggregate precedence is `conflict`,
`unknown`, `not_ready`, then `ready`. Git checks use read-only argv and external
observers persist normalized reason codes, stable identities where required,
and digests rather than raw errors, paths, URLs, or credentials.

Publication is one SQLite transaction that compares all frozen authority,
publishes the complete snapshot, advances the lifecycle pointer, settles the
attempt, and observes the operation receipt. Authority drift settles a
conflict without partial publication. Restart reconciliation classifies an
interrupted in-progress attempt as ambiguous; exact request replay returns its
durable receipt instead of repeating an unproven observation.

Enabling requires the latest complete effective snapshot to be ready and no
recheck to be active. Disabling is always legal and does not alter existing run
authority. Manual and automatic admission first receive a bounded eligibility
token, then SQLite revalidates the lifecycle version, snapshot identity and
digest, profile/binding identity, and configuration authority in the same
transaction that creates or reserves the run. Any lifecycle, profile,
configuration, readiness, or recheck change therefore fences new admission;
already admitted runs continue under their frozen authority.

Removing a repository is a separate Controller-wide mutation lane, not a
lifecycle toggle or a generic configuration edit. An immutable removal draft
binds the exact disabled incarnation, profile/binding digests, lifecycle
version, desired generation/digest, and configuration authority version.
Validation fails closed while any run, onboarding or repository operation,
slot, execution lease, heavy permit, scheduling ownership, cleanup/source-sync
residue, configuration mutation/recovery, drift, or desired/effective/live
convergence conflict remains. Removing the final profile additionally requires
automatic admission to be disabled.

Apply atomically persists a `remove_repository` accepted receipt and removal
intent before delegating the exact one-profile candidate to the existing
configuration generation service. The incarnation remains current but fenced
in `removal_pending_convergence` after file publication. Only a fresh worker
observation of the exact resulting generation/digest atomically marks the
intent observed, settles the receipt, and tombstones that incarnation. Current
repository collections hide tombstones; historical runs, snapshots, attempts,
receipts, configuration generations, audit evidence, local paths, credentials,
and external GitHub/Linear resources are never deleted by retirement. A later
configuration rollback cannot clear a tombstone. Reintroducing the same
canonical repository through onboarding creates a fresh disabled incarnation,
publishes a fresh readiness snapshot, and inherits no prior readiness or
lifecycle intent.

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

A terminal abandoned run has one narrower operator recovery when its frozen
source checkout was relocated and ordinary cleanup can no longer reach the
registered worktree. The current configured controller operator is authorized
before any run or receipt lookup, then must also satisfy the frozen run's exact
trusted-operator scope. A read-only preview proves the exact current
cleanup-residue attention, observed abandon action, absence of execution
authority, frozen-source unavailability, and one canonical same-UID
replacement checkout whose common directory still registers the exact owned
worktree, branch, and candidate head. Apply atomically records a common run
receipt and schema-v45 recovery intent before any Git write. The caller-supplied
replacement path is never persisted or rendered; only digests and stable local
identity evidence are durable.

Recovery is eligible only after ordinary evidence already marks the local
branch ownership and cleanup deleted and the local ref is absent. Its closed,
argv-only sequence repairs the exact worktree link, proves the persisted
candidate object and exact symbolic worktree HEAD, compares index, tracked,
untracked, and ignored state to the candidate, and rejects any per-worktree
merge, cherry-pick, revert, rebase, sequencer, bisect, autostash, or unexpected
administration state. External filter configuration and submodule index entries
are also ineligible, so read-only proof cannot start configured filter or nested
Git processes. The safe topology has a locally available commit in a safe
`ORIG_HEAD`; its bytes and optional safe `COMMIT_EDITMSG` bytes are digest-bound
across preview, effects, and replay. Recovery durably authorizes detaching only
that owned worktree HEAD to the candidate, then removes the exact clean
worktree without force.
Every Git invocation disables hooks, filesystem monitoring, promisor lazy
fetch, and replacement-object semantics; it never mutates a local or remote
branch ref. Every intent/effect boundary is monotonic and replayable.
Before and after each effect, and again before settlement, the application
re-observes and compares the persisted replacement-path, filesystem-identity,
origin, registration, branch, head, and clean-status digests. An absent owned
worktree is adopted only after the replacement common directory proves its
registration absent while the branch ref remains absent. Remote refs, pull
requests,
artifacts, source contents, configuration, lifecycle, and frozen run authority
are outside this recovery. Success updates the normal ownership and cleanup
rows to `deleted`, so repository-removal validation clears through its existing
guard rather than a bypass. The schema-v45 recovery intent is an
`owned_resource_cleanup` integrity-registry source; every insert, stage update,
or delete advances the Controller integrity generation.

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

Integrity maintenance is owned by the outer bounded worker, not by an
individual concurrent dispatch. Once any member of the current bounded batch
returns, the worker stops refilling, lets every existing sibling finish without
cancellation, consumes every buffered result, and waits for dispatch-deferred
lease and scheduling cleanup before advancing one SQLite-only integrity batch.
New dispatch admission resumes only after that quiescent maintenance boundary.
An errored batch joins its siblings and reports the error without publishing
maintenance evidence.

Worker runtime liveness is a separate application contract from workload
activity and configuration convergence. After strict configuration, process
lock, SQLite compatibility, supervisor fencing, and the policy-appropriate
production dispatcher construction succeed,
the active supervisor publishes schema-v3 private heartbeat evidence
immediately and on a fixed 15-second cadence. This is normally the automatic
worker; the mutually exclusive manual `linear start` and `controller run`
supervisors publish the same process-bound evidence for their own lifetime.
Activity transitions may publish immediately but do
not reset that cadence. The single supervisor heartbeat covers all bounded
repository dispatches; there is no per-run heartbeat. Publication failure
cancels and joins dispatch and fails the worker nonzero, while SQLite workflow
state and ordinary restart reconciliation remain recovery authority.

Disabled automatic admission fences only the normal Linear Todo-admission
path; it does not terminate the supervisor. The disabled worker retains the
ordinary process lock, heartbeat, onboarding-only dispatcher, and quiescent
configuration/integrity maintenance for an indefinite lifetime. A valid
version-5 configuration with no repositories adopts one durable count-zero
lifecycle baseline. That singleton binds the exact adoption-time configuration
generation, digest, authority version, and deterministic empty profiles digest;
it creates no repository lifecycle or readiness rows. The generation anchor is
immutable and must remain a retained ancestor when configuration advances. It
never constructs the
normal admission fallback or reads/checks its credential. Idle dispatch returns
the bounded no-candidate outcome. An already accepted onboarding may use its
existing operator-authorized Linear label/readiness effects, but disabled mode
serializes every opportunity at capacity one so the same saga cannot be
executed by sibling slots. Disabled `--once` executes one such bounded
onboarding/integrity opportunity and exits with the ordinary one-shot reason.

The heartbeat atomically replaces the prior private telemetry leaf and binds
the worker instance, PID, OS process-start identity, binary build identity,
exact loaded configuration digest, sanitized activity, the last completed
cycle outcome, last queue-decision reason, worker-owned next admission
evaluation, and observation time. It is not an append-only audit stream.
Onboarding cycles retain the closed `onboarding_accepted`,
`onboarding_running`, `onboarding_waiting_for_operator`,
`onboarding_conflict`, and `onboarding_ready_disabled` outcomes. Waiting and
conflict project a parked supervisor; the other three preserve a running
supervisor. The same classification is reconciled into worker runtime activity
evidence. No other onboarding status or arbitrary prefixed value is valid
heartbeat authority.
Schema-v2 remains valid for liveness and activity while its newer cadence
fields project as unknown. Schema-v1
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
one exact bounded byte payload, then holds the private filesystem mutation
authority while the configuration adapter retains those same bytes and
exclusively publishes a baseline-binding intent,
SQLite durably prepares the matching database anchor, and only then may the
private locator be published. Startup proves a locator or pre-locator binding
target's persisted device/inode identity and prepared binding on one query-only
SQLite connection before enabling writes or migrating that same connection.
The adapter obtains SQLite's actual main-database VFS file descriptor and
`fstat`s it. Every physical connection and idle-pool reuse, plus both sides of
each transaction boundary, direct query or effect, and prepared-statement
effect, and each row-consumption step rechecks that VFS descriptor and the
persisted pathname identity, current-user ownership, single-link state, and
private mode. A
replacement during an otherwise successful effect is therefore returned as a
failure before the application may perform its next side effect. There is no
path-based reopen between proof and effects, and every later production
composition reopens only with the persisted identity constraint.
That proof accepts only the configuration-authority
schema floor through the binary's supported schema, allowing a trusted older
store to receive normal forward migrations while rejecting pre-authority and
newer unsupported stores. SQLite then atomically assigns one baseline generation without
rewriting the live file. The mode-`0600` baseline binding and locator beside the
configuration bind the canonical live path to its owning database path and
exact private file identity so a later invalid or
database-path-drifted file cannot create, redirect, or migrate an attacker-
selected store. Raw
generation payloads remain mode-`0600` beneath current-user mode-`0700`
authority and generation directories. Every trusted locator, binding, or raw
read revalidates those non-symlink private ancestors before accepting a leaf;
the bounded leaf read also revalidates the opened inode and current pathname's
owner, private mode, and single-link identity after reading. First creation and
every retry fsync the parent entry for each private authority directory while
pinning and revalidating both the child and parent directory identities.
SQLite contains metadata, receipts, and sanitized events only. Current plus
nine recent settled payloads are retained, while current and
unresolved evidence are never pruned. Deletion first acquires a durable digest
claim in SQLite; apply acceptance checks that claim in its transaction, so a
digest cannot become accepted while its raw leaf is being removed. Raw target
publication, prune deletion/metadata completion, and live replacement also
flock the stable filesystem-root inode. The authority does not depend on any
user-replaceable lock-file, configuration-parent, or authority-subtree
pathname, so replacing a descendant cannot give a second Controller process a
parallel mutation authority. An apply therefore cannot recreate a claimed raw
leaf between deletion and metadata settlement. Startup finishes interrupted
claims idempotently and removes raw digest leaves that have no retained SQLite
generation anchor. Existing identical raw, baseline, and locator publications
re-sync their parent directory before success; an already-absent prune retry
does the same before settling metadata. Authority-directory sync pins an opened
directory descriptor and revalidates its current pathname, owner, mode `0700`,
and inode while the exact leaf descriptor remains open; the leaf descriptor and
pathname are revalidated only after that sync. Removal similarly proves the leaf
is still absent after sync. Nested raw reads pin and revalidate both the
authority and generation directories. Exclusive publications use an OS
no-replace rename, so the fully synced temporary inode becomes the single-link
final inode without a temp/final hard-link crash window. Restart cleanup
removes interrupted pre-publication temporary leaves while holding mutation
authority.

The presentation-independent apply service authorizes from the committed
desired generation's configured operator, strictly validates current-schema
candidate bytes, compares expected generation and digest, protects every
nonterminal run's frozen repository, Linear task-source, and configured-
operator authority. The
current request's controller scope is resolved again after reconciliation,
because reconciliation may commit an already-accepted operator transition.
An accepted receipt is not a settled replay: an exact retry first reconciles
the durable intent and then returns the observed result, including when that
generation changed the configured operator.
Only then does the service retain the target and commit one apply intent and
operation receipt before a same-
directory atomic exchange. The file adapter rechecks the bound database path
after intent persistence and immediately before and after that exchange; a
database replacement fences or restores the live-file effect. The exchange
captures the exact displaced inode;
an unexpected concurrent edit is restored instead of overwritten. The
captured parent remains as private operation evidence until directory sync and
exact reread prove the target. Removing the staged leaf also requires a proven
parent-directory sync; an interrupted or failed cleanup remains an error until
reconciliation repeats that sync. Startup and pre-apply reconciliation settle an
interrupted intent from exact parent/target evidence or an ambiguous third or
unsafe observation; drift is never adopted or overwritten. A matching fresh
heartbeat durably selects only the current desired generation as effective.
If a new generation returns to a digest already loaded by the fresh worker,
the apply response performs that correlation immediately instead of requiring
another projection poll or restart.
The resulting finite projection is
`ready`, `restart_required`, `starting`, `stale`, `offline`, `unknown`, or
`conflict`, separate from worker activity and capacity.

Safely readable external drift has one narrower recovery action. After
Controller authorization, status may expose only the current desired
generation/digest, configuration authority version, and exact observed live
digest needed for `restore_configuration` compare-and-swap. The command
reauthorizes and recomputes that evidence immediately before atomically
persisting one recovery intent and generic Controller-scope receipt. Apply and
restore acceptance are mutually exclusive in SQLite and share the stable
filesystem-root mutation lock. Restore exchanges the exact accepted observed
file for the retained exact desired bytes; it neither creates a generation nor
adopts or retains external bytes. Startup and every pre-mutation reconciliation
resume only an already-accepted observed digest under the same operation ID.
Exact desired evidence settles success; a third digest or unsafe or
contradictory stage settles ambiguous, preserves the required private evidence,
and keeps admission fenced. Successful settlement records one
`drift_cleared` transition, advances configuration authority version, and then
reuses the ordinary runtime convergence projection. A later recurrence must
first establish newer drift authority and cannot replay the earlier receipt.

The typed lifecycle adapter adds one Controller-wide durable normal or
rollback-origin scalar draft without adding another configuration transaction.
Repository removal uses an exclusive source-bound draft lane and cannot overlap
that scalar draft, apply, or recovery authority. Controller authorization happens
before every draft lookup. Revision compare-and-swap plus the last closed edit
digest provides exact lost-response replay, while a different edit at the same
revision conflicts. SQLite stores only the nine allowlisted timeout/admission
scalars, their integrity digest, finite lifecycle, sanitized validation and
preview metadata, and the resulting generation/receipt binding. Candidate
bytes are never stored with the draft: validation, preview, and apply reread the
exact retained base-generation raw bytes and deterministically materialize a
schema-v5 candidate in memory. An unchanged projection returns the exact base
bytes and reaches the generation service's same-digest no-op path.

Rollback discovery authorizes before generation-history or raw-evidence reads
and returns only the exact current desired authority plus bounded sanitized
superseded sources. A safely committed source does not require an effective
worker observation before a later apply supersedes it; when present, the
effective observation must fall between commit and supersession. Apply-origin
sources also require matching committed intent and successful receipt evidence,
while a baseline source requires its matching adoption anchor. The configuration
adapter strictly projects the nine scalar settings from retained schema versions
1 through 5, including version-3
singleton-to-heavy-capacity semantics, without resolving a historical external
registry, reading credentials, or performing network or supervisor work. Draft
creation rechecks source retention and current desired CAS in the same SQLite
write transaction; a concurrent prune claim conflicts, while pruning may resume
normally after the source identity and typed settings are durably bound.

Validation returns only closed field IDs, finite reason codes, and severity.
Semantic preview returns allowlisted boolean, duration, and bounded-integer
before/after values plus finite restart, convergence-fence, frozen-run, and
capacity-drain impacts. Its digest binds draft/revision, base and candidate
digests, immutable rollback source when present, semantic changes, impacts, and
current configuration authority. Apply
reauthorizes and recomputes all of that evidence, marks the draft applying, and
delegates to the existing configuration apply service. The existing
`apply_configuration` receipt remains the sole apply operation identity. Normal
apply retains its pre-v33 private identity for migration replay, while a
rollback anchor distinguishes every exact rollback source
while its request digest remains the candidate digest. A real rollback records
the immutable source on the new applied generation before live replacement; a
same-digest rollback creates no generation but retains source-bound draft and
receipt evidence. Historical non-editable authority is never materialized.
Response-loss and restart replay reconcile the same generation rather than
creating another. Apply never starts, stops, or restarts a worker.

One explicit schema-migration lane bridges readable inline configuration
versions 2 through 4 to the current version 5 mutation boundary. Preview is
read-only and requires the retained desired bytes, exact desired/effective/live
convergence, a fresh matching worker, and no unresolved configuration or
repository-topology mutation. The private configuration adapter changes only
the schema number, the already deterministically derived Controller-wide
operator, and version
3's retired singleton capacity leaf. It proves decoded controller, Linear,
GitHub App, repository, and automation authority remains functionally equal;
repository profile, registry, and binding digests remain exactly equal. GitHub
App raw JSON whitespace is not authority and its raw formatting digest is not
a migration invariant, while its complete decoded profile and credential
reference are. Raw profile bytes, credential references, paths, and private
file identities never enter the preview.

Apply recomputes the candidate from the exact retained legacy generation and
requires its generation, digest, schema, configuration-authority version,
candidate and migration digests, complete preview digest, configured requester,
and a bounded stable request ID. SQLite atomically excludes active scalar
drafts, recovery, repository removal, onboarding, and another apply. The
existing `apply_configuration` receipt, durable apply intent, immutable raw
generation, and filesystem exchange remain the sole effect and crash-recovery
authority. The resulting generation records `schema_migration` provenance plus
the request and preview binding before live replacement. Exact response-loss
replay deliberately resolves that receipt from the superseded legacy parent
after desired authority has already advanced to version 5; current-schema and
current-generation guards run only for a new effect. Receipt identity binds the
request, preview, and candidate before legacy raw is required. An observed exact
replay therefore survives later pruning of the superseded source raw; an
accepted replay loads the retained target generation and delegates the existing
apply reconciler. A successful apply returns `restart_required`; only a fresh
worker observation of the exact version-5 digest completes convergence. Version
1 is excluded because its repository authority depends on an external registry
file.

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
source state remains untouched and produces sanitized attention. For the
single source-relocation topology described under cleanup ownership, the worker
never guesses a replacement path; only an explicit typed operator preview/apply
can resume the local cleanup.

**Key invariants**

No stash, reset, rebase, checkout switch, force deletion, or user-resource
adoption.

## 9. Adapter Modules

### SQLite

`internal/adapters/sqlite` is the durable store and migration owner. It enforces
foreign keys, busy timeout, expected-state CAS, unique ownership/idempotency
constraints, leases, atomic evidence/transition handoffs, and sanitized
inspection. The current schema is version 47; migration history is code, not a
human workflow API.

### Git and worktrees

`internal/adapters/git` provisions and validates isolated worktrees, observes
branch/base/head/status, creates controller-authored commits, publishes explicit
refspecs, verifies accepted external merges, synchronizes source checkouts,
cleans resources, and performs the exact source-relocation worktree repair
recovery. Commands are argv-only through the managed process adapter;
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

Controller-managed Codex commands never use
`--dangerously-bypass-approvals-and-sandbox`, `--ignore-rules`,
`--skip-git-repo-check`, `resume --last`, or `--strict-config`. They pass prompts
through stdin, keep JSONL stdout separate from stderr, validate versioned final
messages, and preserve unknown JSONL event types as telemetry. Repository design
profiles and checkpoints are not injected into these commands or prompts.

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
Its separate historical scalar projection reads only the strict in-memory
document shape needed for rollback and never traverses legacy authority paths.
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

### Local binary replacement

The repository-owned `scripts/local-agentctl-upgrade.sh` is an operator-driven
host adapter, not Controller workflow state and not a self-update service. It
accepts one exact local Git commit, copies it into an independent local clone
without shared object storage, builds and verifies the detached clean checkout,
and stages one candidate in a private single-active bundle.
The candidate's structured build identity binds product version, clean VCS
revision and time, modified state, and the schema version exported by the
SQLite adapter's single supported-schema source. The same identity is emitted
by the worker heartbeat; the plain `agentctl version` contract remains
compatible.

Replacement is fenced by explicit LaunchAgent or LaunchDaemon selection,
four-label/domain observation, and the existing process-lifetime worker lock.
The adapter creates a consistent SQLite snapshot through the pinned driver's
online backup API, then persists replacement intent before atomically replacing
the same-filesystem binary. The snapshot is evidence only and has no restore
interface. Host-upgrade phases live in an atomically synchronized filesystem
journal because binary and database schema compatibility can span Controller
database versions.

Durable `bootstrap_intent` is the irreversible authority boundary. Before it,
the exact previous binary may be restored only while the worker remains fenced
and database identity/schema are unchanged. After it, no managed binary or
database rollback is allowed: missing launchd responses, worker identity
conflicts, or readiness failures preserve the bundle for attention. Cleanup is
authorized only by an exact candidate/heartbeat/configuration/schema/supervisor
match plus healthy Controller readiness, or by an equally verified pre-intent
rollback. It writes one current-installation manifest and removes only the
closed set of artifacts owned by that completed bundle.

An active post-bootstrap attention may create one managed successor only when
the installed candidate, selected running supervisor, worker process identity,
fresh heartbeat, loaded configuration, database topology, and binary health
still verify, while Controller readiness is explicitly `not_ready`. The
successor is built from another exact clean local revision through the same
independent-clone gate. A durable predecessor intent fixes one successor
identifier and revision before staging; atomically replaced journals link both
bundles and terminalize the predecessor before the single active pointer moves
to the prepared successor. Every transition is replayable, so response loss
cannot create a second successor or ambiguous pointer authority. The
predecessor bundle then becomes immutable retained evidence and is never a
cleanup target.

The compatibility-only `integrity_convergence_exhausted` attention is eligible
under that same managed-successor boundary. It is derived only from a valid
current v1 observation linked to a published convergence-attempt-eight scan
whose exact seven registered families are all incomplete
`unknown/convergence_bound_exhausted`, while the current integrity generation
is equal to or newer than the published generation. The observation digest,
current pointer, registry order, scan link, family set, and coverage/count
evidence must all verify. Generic `integrity_pending`, partial or malformed
evidence, and unrelated unknown results never gain successor authority.

The compatibility-only `integrity_publication_not_stable` attention is limited
to an already-installed schema-v43 candidate. It requires the valid current
pointer to a complete attempt-eight `ready` publication with the exact seven
registered families, a second earlier publication with the same complete
shape, intervening `source_generation_advanced` scan evidence, and a later
active or superseded scan proving that Controller mutations immediately made
the latest publication stale. Observation digests, scan identities and
boundaries, registry order, family evidence, finding absence, and generation
ordering must all verify. Generic stale readiness, one publication, partial or
malformed evidence, and ordinary `integrity_pending` remain ineligible.

The successor binds its previous-binary evidence to the failed installed
candidate, never to the predecessor's pre-bootstrap binary. Its replacement
therefore reuses the normal absent-supervisor, worker-lock, current
configuration/database, online snapshot, and newly confirmed encrypted-full-
backup gates. Pre-intent successor rollback can restore only that failed
candidate; after the successor's own `bootstrap_intent`, rollback is again
permanently forbidden. Successful successor cleanup removes only the successor
bundle and active pointer while retaining the terminal predecessor lineage.

One narrower recovery precedes successor preparation when an operator-confirmed
local file relocation changed the database inode at the unchanged canonical
path. It is available only for the same post-bootstrap Controller-readiness
attention reasons as an ordinary managed successor, but requires the selected,
opposite, and legacy supervisors to be absent and the installed binary to remain
the exact predecessor candidate. Ordinary startup remains bound to the existing
locator and continues to reject the replacement inode.

The read-only recovery preview derives the old locator identity from the
predecessor journal and private authority locator. A query-only verifier pins
the replacement file's owner, private mode, single link, canonical path,
schema, SQLite and foreign-key integrity, internal configuration paths, desired
configuration authority, readiness reason, and stable database/WAL content.
Readiness normally remains an exact match with the predecessor's durable
failure reason. The sole additional relationship is a durable predecessor
`integrity_conflict` followed by `integrity_pending` when a valid current
published observation is older than a strictly newer integrity generation.
That stale observation cannot assert current Controller readiness. The typed
relationship, both generations, observation consistency, predecessor failure,
replacement readiness, predecessor binary, supervisor absence, and exact
successor revision are all bound into the private preview evidence and digest.
Only the digest and required confirmation names are rendered; paths,
device/inode values, observation identities, generation values, locator bytes,
configuration digests, database contents, and credentials remain private.

Prepare requires explicit relocation and encrypted-full-backup confirmations,
revalidates the complete preview around candidate verification, and durably
records one recovery intent and successor identity before locator mutation. The
configuration filesystem adapter holds its stable mutation lock, pins the
replacement database descriptor, accepts only the exact old or already-
recovered locator, and atomically changes only the locator's database identity
with directory synchronization. The journal then binds the observed
replacement identity and publication before the existing independent-clone
successor staging and active-pointer transfer. Restart distinguishes the old
locator, the exact recovered locator, and every unexpected third identity;
identical replay resumes one successor while generation, observation,
configuration, content, binary, supervisor, or locator drift preserves all
evidence.
Successful successor cleanup retains the failed predecessor's complete recovery
record.

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

### Routine query and planned local operator interface boundary

The implemented routine query family is a versioned, bounded application
contract for Controller Overview, queue, compact runs and fixed delivery gates,
active attention, repositories, onboarding, and settings. Every query first
authenticates the complete configured operator, applies scope before collection
shape, and returns only allowlisted typed fields with sanitized response
digests. Routine reads do not reconcile, refresh external systems, acknowledge
attention, or advance workflow authority. The activity/history contracts
extend this same presentation-independent boundary.

The implemented activity family is a separate append-only explanatory
projection. It uses deterministic identities over schema version, private
source kind, immutable source identity, and finite semantic event kind. Public
snapshots expose only the eight closed categories, finite actor/reason/event
classifications, authorized target, state/version changes, bounded typed links,
times, evidence digests, and immutable per-event coverage class. The private
source key and target binding never leave persistence. Exact replay is
idempotent. Equivalent current/backfill discovery preserves the first persisted
coverage classification; source or semantic snapshot drift conflicts and never
overwrites history.

Meaningful SQLite-owned source facts and their activity rows commit in one
transaction. A settled operation has exactly one primary activity link: a
specific run, repository, onboarding, or configuration event owns the link
when the settlement transaction produces that event; otherwise the receipt
produces a generic operation event. Pending receipt phases remain visible only
in bounded operation history. Runtime worker classification changes reconcile
idempotently outside source transactions; unchanged heartbeat observations do
not append events. Runtime lag or conflict degrades coverage without granting
workflow authority.

Activity list/detail and operation-history list are authorization-first,
side-effect-free reads. Activity pages fix an ingestion watermark and bind the
opaque cursor to schema, authorized-scope digest, filters, keyset position, and
that watermark. Receipt pages bind the same authority/filter evidence to the
immutable accepted-time ordering boundary, so later monotonic receipt advances
do not reorder a page sequence. Automatic legacy reconstruction is bounded,
SQLite-only, resumable per source, and worker-fair. Coverage distinguishes
`complete`, `backfilling`, `degraded`, `unknown`, and `conflict`, and always
discloses that non-persisted legacy worker/runtime transitions and repository
intent history cannot be reconstructed, while legacy attention resolution is
only partially reconstructable and admission-capacity decisions are not
backfilled. A later successful indexing cycle clears a transient degradation;
an immutable-source conflict remains explicit. Activity, coverage, cursors,
and projection digests are never workflow or mutation authority.

Controller-wide audit-integrity observation is a separate persistence-only
projection. Its versioned registry is closed to exactly `storage_schema`,
`run_delivery`, `operation_activity`, `configuration`,
`repository_onboarding`, `scheduling_admission`, and
`owned_resource_cleanup`. SQLite triggers advance one monotonic source
generation and the affected private family revision for every registered
source-table mutation. Integrity scan, progress, finding, observation, and
current-pointer writes are excluded, so maintenance cannot invalidate itself.
For `activity_runtime_state`, an update that changes only `observed_at` is also
excluded: freshness remains queryable but does not change the classified
runtime authority. Insert, delete, or any change to source kind, source
identity, classification, source-evidence digest, status, or reason still
advances the `operation_activity` revision.

The automatic worker gives onboarding and, only while automatic admission is
enabled, normal admission their opportunity first. After the current bounded
dispatch batch is fully quiescent, it advances
at most one SQLite-only integrity family batch without a heavy permit, then
resumes dispatch refills. One durable scan lane owns its registry version, target
generation, cursor, lease, checked revisions, deterministic findings, and
committed progress. Restart reuses committed progress. Publication requires
all seven results and an unchanged source generation in one transaction; a
racing mutation supersedes and requeues the scan. Immutable family states are
`ready`, `not_ready`, `unknown`, and `conflict`, aggregated in that precedence.
A prior observation remains history, but its effective readiness becomes
`unknown` immediately when the registry or source generation advances.
Ordinary progress remains one family per cycle. When repeated source advance
reaches the fixed convergence-attempt bound, one final SQLite-only transaction
checks all seven fixed families and uses the same mutation-fenced publication
path. It preserves each real family result and performs no external effect,
heavy work, or retry loop; a mutation racing that fallback supersedes it rather
than publishing stale readiness.

The configured operator may explicitly request the closed
`recheck_integrity` Controller operation. One versioned private request key
binds the exact requester and caller request ID; exact replay returns the same
common operation receipt and immutable scan binding. Acceptance inserts and
applies that receipt before capturing the resulting source generation, then
atomically supersedes any older automatic scan and binds the deterministic
replacement scan for that post-receipt generation. At most one explicit
recheck is active. Neither the CLI nor any caller can select families, rules,
SQL, bounds, generations, scan identities, repair, force, or skip behavior.

Automatic maintenance and explicit requests share the same scan cursor, lease,
seven family checks, convergence bound, and publication path. A competing
unexpired lease leaves the receipt applied and pending for worker continuation.
A later registered-source mutation supersedes the bound scan and atomically
settles the receipt as observed/conflict without rebinding. Restart resumes
only the exact committed scan and cursor; replay never creates replacement
work for an already applied receipt.

Successful explicit finalization uses one schema-v41 persistence-private guard
inside the exact SQLite publication transaction. It suppresses generation
advancement only for settling that one `recheck_integrity` receipt and
inserting its deterministic generic operation activity event and primary link.
The transaction then reruns `operation_activity`, replaces only that scan's
pre-final family evidence, validates the receipt, activity, link, generation,
guard identity, and absence of unrelated mutation, publishes the observation,
advances the current pointer, settles the recheck binding, and removes both
the active pointer and guard. Any mismatch or interruption rolls back the
entire bundle, leaving the receipt applied/pending. Residual guard evidence,
a published scan with a pending receipt, or a succeeded receipt without its
exact observation is conflict evidence and is never automatically repaired.
Operation success is distinct from readiness: the bound trusted observation
may be `ready`, `not_ready`, `unknown`, or `conflict`.

The summary and affected-scope detail contracts authenticate the complete
configured operator before persistence lookup, counting, ordering, or cursor
construction. Findings expose only opaque identity, family, finite reason,
authorized `controller`, `repository`, `run`, or `onboarding` scope, observation
time, and bounded classification metadata. Untrustworthy narrower bindings
fall back to Controller scope. Cursors bind the immutable observation digest,
authorization digest, filter digest, and keyset position. These projections are
explanatory only: they do not block, authorize, repair, delete, or otherwise
change existing workflow or external effects. The receipt-backed recheck is
implemented through the same application boundary; presentation adapters
remain follow-up work.

The planned local operator interface is a TUI over these Controller-owned
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
presentation. Repository onboarding and configuration transactions use those
same contracts. CLI, TUI, and any future HTTP adapter call the same typed
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

Repository onboarding is a persisted Controller saga. Each step row owns a
positive durable attempt number. Resume atomically re-arms the current failed
or pending step and advances that attempt; immutable activity remains
explanatory history rather than retry authority. Attempt 1 retains its legacy
activity identity, while later attempts use deterministic attempt-qualified
identities and evidence digests. A new-project flow
begins from an existing empty GitHub repository, creates the managed local
checkout and initial base revision, creates or adopts the exact Linear
`repo:<slug>` label, validates the repository profile, applies configuration,
and observes worker readiness. An existing-project flow validates and adopts a
matching local checkout and GitHub origin without rewriting user-owned Git
state. Partial progress is resumed or reconciled; external resources are not
destructively rolled back by implication.

Both implemented kinds persist only sanitized path and observation digests in
SQLite. Before configuration apply, an existing checkout path or derived
managed source path remains only in a private Controller-owned, closed
kind-discriminated input leaf. Preflight is read-only across local paths, Git,
GitHub, Linear, configuration, lifecycle, and readiness authority. Start
rechecks the exact preflight before atomically accepting an onboarding
operation receipt.

`existing_checkout` retains its seven-step topology and never mutates the
user-owned checkout. `empty_repository` uses a ten-step topology: Controller
roots, managed source creation, deterministic empty root revision, guarded
initial-base publication, then the same Linear label, source-bound
configuration addition, fresh worker convergence, disabled lifecycle,
complete readiness, and settlement tail. The initial revision fixes the
Controller author identity, empty tree, message, base branch, and accepted UTC
second. Host Git transport uses the exact credential-free GitHub SSH remote and
a plain non-force base refspec. Every push outcome is followed by a complete
remote-ref reread and an exact GitHub App base-ref read before local
remote-tracking settlement. A restart resumes the first unsettled kind-specific
step; a proven published ref is adopted without another push, ambiguous or
divergent evidence fails closed, and partial progress is never implicitly
rolled back. Both kinds complete only as `ready_disabled`.

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
| `owned_resources`, `cleanup_results`, `cleanup_source_recovery_intents` | Resource ownership, per-resource cleanup progress, and the path-private monotonic authority for exact abandoned-run recovery after a source relocation |
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
| `activity_events`, operation links, backfill progress, and runtime classification state | Immutable sanitized activity snapshots, one-primary-event receipt correlation, stable ingestion watermarks, bounded restart progress, and explicit coverage evidence; never workflow authority |
| integrity registry, generation/revision, scan, explicit-recheck binding/active/guard, checked-family, finding, observation, and current-pointer tables | Closed SQLite-only invariant registry, semantic mutation coverage with a freshness-only runtime-state exception, one restart-safe bounded scan lane plus a single transaction-fenced full-family convergence fallback, receipt-bound post-acceptance rechecks, exact transaction-only finalization suppression, immutable mutation-fenced observations, and sanitized explanatory findings; never workflow or repair authority |
| configuration generation, authority, apply/recovery-intent, and convergence tables | Immutable desired/effective metadata, one Controller-wide CAS mutation authority, crash reconciliation state, optional immutable rollback-source identity, and meaningful sanitized transitions; raw desired bytes remain outside SQLite and external bytes are never stored |
| `configuration_drafts` | At most one active Controller-wide normal or rollback-origin typed draft, revision/edit replay authority, immutable rollback source when applicable, sanitized validation/preview evidence, and generation/receipt settlement; no raw candidate, path, identity, or credential authority |
| repository lifecycle, readiness, recheck, and removal tables | Immutable incarnation history, one current canonical/profile/binding authority, complete readiness evidence, exclusive source-bound removal draft and accepted/applied/observed settlement, and tombstone evidence; no external-resource deletion authority |
| repository onboarding and onboarding-step tables | One active canonical repository/source-path digest, one of two closed kind-specific step plans, a positive durable current-step attempt, sanitized preflight/preview bindings and exact initial SHA, ordered intent-before-effect settlements, monotonic partial-progress recovery, and final disabled incarnation/readiness evidence; exact source paths and raw Git/remote output remain outside SQLite |

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
- GitHub API writes require a narrowly permissioned selected-repository App;
  empty-repository base publication is the separate guarded host-SSH Git
  transport described above.
- Unsafe or ambiguous configuration recovery, explicit integrity recheck,
  the local TUI, optional HTTP/Web adapters,
  notification transport, Hermes runtime integration, public API, webhooks,
  and multi-tenant authorization are not implemented.
- External live E2E acceptance remains restricted to isolated fixture
  repositories. The automatic-delivery acceptance and repair-aware independent
  review are complete; future live gates must follow the staged Controller and
  operator-interface contracts without broadening production authority.
