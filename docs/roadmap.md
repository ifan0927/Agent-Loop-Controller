# Roadmap

## Product Vision

Agent Loop Controller should make a local coding-delivery loop feel like one
coherent product across Hermes, Linear, Codex, and GitHub without making an LLM
the workflow authority.

The intended experience is:

- a human operator or a future conversation adapter shapes a coding-ready task
  and observes progress;
- Linear owns task definition, priority, acceptance criteria, and lifecycle;
- the controller admits work, persists authority/evidence, and orchestrates the
  one legal next action;
- Codex implements, resumes, repairs, and performs a fresh independent review;
- GitHub owns repository delivery, CI, human review, and merge evidence;
- the configured human operator makes ambiguous product decisions and final
  review/approval decisions;
- restarts and partial failures recover from durable evidence rather than human
  reconstruction.

The product is local-first and TUI-first. Its application contracts remain
replaceable enough for future adapters without making a terminal UI, browser,
Hermes, or an agent the source of truth.

## Guiding Principles

- **Deterministic control, nondeterministic execution.** The controller is a
  state machine; Codex is an executor behind validated contracts.
- **Human-gated authority.** No agent or App approves its own work, resolves a
  human conversation, or silently chooses an ambiguous task decision.
- **Exact evidence.** Verification, review, CI, approval, and merge are bound to
  exact Git heads and immutable external identities.
- **Local-first and recoverable.** SQLite state, isolated worktrees, private
  artifacts, and resumable processes make host restarts routine.
- **Narrow side effects.** Persist intent, call one typed operation, observe the
  result, and reconcile ambiguity idempotently.
- **One canonical task tracker.** This file records direction and milestone
  status; implementation slices and active acceptance work live in issues.
- **No speculative platform.** Add abstractions only when a current workflow,
  test, or committed product boundary requires them.

## Completed Milestones

### Completed: deterministic local execution foundation

- Pure domain task/outcome/state contracts and embedded structured-output
  schemas.
- Managed process, Git workspace, isolated worktree, artifact ownership, and
  repository-owned verifier boundaries.
- Resumable Codex implementation and fresh ephemeral read-only review.
- SQLite state, transitions, attempts, exact-head evidence, leases, CAS, and
  restart recovery.
- Disposable local labs, deterministic fixtures, race/vet/security scan gate,
  and CI using the same repository verification script.

### Completed: production MVP delivery vertical slice

- Versioned multi-profile configuration with one selected repository per run.
- Direct Linear task read/admission and immutable task/profile snapshots.
- Production coordinator/driver and requester-authorized status/inspection.
- Exact-head branch push, owned PR create/adopt, required GitHub checks, trusted
  exact-head human approval, guarded squash merge, Linear completion
  observation, and ownership-safe cleanup.
- First isolated fixture dogfood through merge and cleanup.

This milestone is recorded by the closed
[production MVP roadmap](https://github.com/ifan0927/Agent-Loop-Controller/issues/1).

### Completed: automatic admission, trusted feedback, and bounded concurrency

- Disabled-by-default Linear Todo admission with deterministic cross-repository
  priority/identifier/UUID ordering and one-candidate reservation decisions.
- Short global admission lease, atomic reservation/mutation journal, one-active-
  run-per-repository slots, generic bounded local-heavy-work permits, durable
  retry schedule, worker, macOS LaunchAgent controls, and non-root headless
  LaunchDaemon supervision.
- Sanitized transport-neutral operator-attention events and queue-decision
  projection.
- Trusted human-review feedback lifecycle, same-session repair, fresh
  review, idempotent GitHub App reply, and conversation-resolution wait.
- Exact merge-SHA source checkout synchronization and partial cleanup recovery.
- Graceful parked-run abandon with proven managed-child termination and guarded
  local/remote ownership cleanup,
  terminal residue attention, repository-slot release, owned repair-push recovery,
  and verified external merge acceptance.
- Typed exhausted-retry recovery with durable operator intent and automatic
  worker resume through the normal driver.
- Deterministic continuous-supervisor restart/fault matrix with a sanitized,
  machine-readable evidence summary for these boundaries.

The implementation child work under the
[automatic-admission roadmap](https://github.com/ifan0927/Agent-Loop-Controller/issues/21)
and the final isolated
[live acceptance](https://github.com/ifan0927/Agent-Loop-Controller/issues/42)
are complete. The accepted run proved automatic admission, trusted inline
repair and reply, an unresolved-conversation worker restart, exact-head human
approval, protected merge, Linear completion, exact source synchronization,
owned cleanup, and non-disclosing retained evidence.

This milestone is recorded by the completed automatic-admission roadmap and its
bounded remediation roadmap
[#45](https://github.com/ifan0927/Agent-Loop-Controller/issues/45).

## Current Product Focus

### Planned: local operator product

The active umbrella is
[#99](https://github.com/ifan0927/Agent-Loop-Controller/issues/99). It replaces
the retired read-only monitoring roadmap with a staged operator product intended
to cover at least 90 percent of routine local work:

```text
completed controller, bounded-concurrency, and authorization foundation
  -> remaining controller operator foundations
  -> local TUI operator console
  -> optional future adapters only when justified
     |- Operator API
     |- Web UI
     `- Hermes
```

`agentctl` is the canonical target executable name. The intended runtime entry
points are `agentctl controller worker` and `agentctl operator`. The currently
implemented `ifan-loop` executable and launchd identity remain a compatibility
surface until the installation-safe migration in
[#104](https://github.com/ifan0927/Agent-Loop-Controller/issues/104) completes.

Implementation work is created one dependency-ready issue at a time. Local
operator identity and application authorization are complete. The next
foundation is operation receipts and legal-action offers. Do not create
speculative TUI, HTTP, or frontend slices before their Controller contracts
exist.

### Phase 1: controller operator foundations

Bounded multi-repository concurrency and local operator authorization are
complete. The remaining
presentation-independent sequence is:

1. operation receipts and legal-action offers;
2. worker heartbeat and runtime observation;
3. configuration generation and compare-and-swap apply;
4. typed configuration lifecycle;
5. repository lifecycle and readiness;
6. restart-safe onboarding saga;
7. existing-checkout adoption;
8. empty-repository initialization;
9. routine Controller projections;
10. activity and audit integrity.

These services own policy, authorization, idempotency, reconciliation, and
sanitized evidence independently of presentation. GitHub repository creation,
source templates, and UI secret provisioning are not part of this phase.

### Phase 2: local TUI operator console

The initial TUI is a presentation adapter in this repository and Go module. It
runs as a separate process from the worker, reads durable Controller state
through typed application projections, and invokes only Controller-owned legal
actions. Closing or crashing the TUI must not stop Controller execution.

When implementation begins, use Bubble Tea v2, Bubbles v2, and Lip Gloss v2.
Do not add those dependencies before an implementation issue requires them.
The likely navigation surface is Overview, Runs, Queue, Attention,
Repositories, Onboarding, Settings, and System/Audit, but stable Controller
projections determine screen details.

The fixed product metric remains ten complete operator-intent scenarios from
[#100](https://github.com/ifan0927/Agent-Loop-Controller/issues/100), worth ten
points each. At least 9/10 substantiates the 90-percent claim; v1 targets 10/10.
A scenario counts only when the complete human goal is supported without raw
SQLite, config-file editing, artifacts, logs, filesystem inspection, or ad-hoc
Controller internals. Manual admission may remain CLI-only and outside the
denominator.

TUI verification starts with Go model/update/application tests and deterministic
View or golden tests, then adds a bounded set of VHS critical-flow acceptance
tests. VHS is development tooling, not a runtime dependency.

### Phase 3: optional future adapters

HTTP, a Web UI, or Hermes may later adapt the same Controller application
contracts when a demonstrated consumer needs browser, remote, conversation, or
integration transport. They are not prerequisites for the local TUI. Future
adapters must not derive policy, own worker lifecycle, or introduce a second
state machine.

GitHub approval and review resolution remain in GitHub. General Linear issue
editing remains in Linear. Privileged installation/upgrade, secret management,
and break-glass recovery remain explicit CLI/operator procedures.

## Near-Term Goals

### Planned: outbound notification delivery

The local operator product has no notification inbox, delivery history, read
state, or inbound chat authority. After the Controller foundations and TUI are
usable, outbound notifications may be planned separately, with Discord as a
possible adapter. Delivery and acknowledgement must remain subordinate to
Controller state.

### Planned: Hermes application integration

Hermes may later use the same authenticated application commands and sanitized
queries for conversation, trigger, status, and notification workflows. It must
not execute Mac shell commands, read worktrees, approve GitHub reviews, resolve
human threads, or own controller state.

### Planned: multi-repository lifecycle and visibility

Bounded multi-repository scheduling is implemented while each run still selects
exactly one repository. The remaining operator foundations and TUI should make
repository onboarding, readiness, lifecycle, capacity, and active-run state
visible without introducing cross-repository transactions or one issue spanning
multiple PRs.

## Longer-Term Direction

### Exploratory: event-driven admission

Linear webhooks or another event source may reduce polling latency after the
same admission eligibility, repository/capacity, signature verification, and
deduplication rules can be preserved. Event delivery must be treated as a hint
to re-read Linear, never as authoritative task content.

### Exploratory: executor and review evolution

Codex models and CLI capabilities may change behind compatibility tests and
versioned command contracts. Implementation resume and fresh independent review
must remain distinct, and a new model cannot be treated as equivalent without
representative evaluation.

## Explicit Non-Goals

### Non-goal for the current product boundary

- Reimplementing Codex reasoning, context management, or memory.
- Treating the controller, Hermes, Codex, or a GitHub App as human approval.
- Automatic review-thread resolution or branch-protection bypass.
- Executing Linear/GitHub/Hermes text as shell commands or verifier definitions.
- Production deployment, destructive data operations, or production recovery.
- Multi-tenant hosted operation.
- Cross-repository atomic transactions or multi-PR issues.
- Automatic prompt, policy, or workflow self-evolution.
- Replacing Linear with a second issue tracker in repository documentation.

## Tracking

Status words in this document are deliberate:

- **Completed**: implemented with deterministic verification; live acceptance is
  named separately when still pending.
- **In progress**: active acceptance/stabilization work with an open tracker.
- **Planned**: intended product direction, not yet implemented or necessarily
  decomposed.
- **Exploratory**: requires product/security design before commitment.
- **Non-goal**: outside the current product boundary.

Detailed implementation state, acceptance checklists, dependencies, and defect
history belong in GitHub issues and pull requests. The current open umbrella is
[#99](https://github.com/ifan0927/Agent-Loop-Controller/issues/99). The completed
trackers [#21](https://github.com/ifan0927/Agent-Loop-Controller/issues/21),
[#42](https://github.com/ifan0927/Agent-Loop-Controller/issues/42), and
[#45](https://github.com/ifan0927/Agent-Loop-Controller/issues/45) retain their
historical implementation evidence. Update this roadmap when milestone meaning
changes; do not copy full issue checklists here.
