# Roadmap

## Product Vision

Agent Loop Controller should make a local coding-delivery loop feel like one
coherent product across Hermes, Linear, Codex, and GitHub without making an LLM
the workflow authority.

The intended experience is:

- I-Fan and Hermes shape a coding-ready task and observe progress;
- Linear owns task definition, priority, acceptance criteria, and lifecycle;
- the controller admits work, persists authority/evidence, and orchestrates the
  one legal next action;
- Codex implements, resumes, repairs, and performs a fresh independent review;
- GitHub owns repository delivery, CI, human review, and merge evidence;
- I-Fan makes ambiguous product decisions and final review/approval decisions;
- restarts and partial failures recover from durable evidence rather than human
  reconstruction.

The product is local-first today, but its application contracts should permit a
future authenticated operator interface without turning the browser, Hermes, or
an agent into the source of truth.

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

### Completed: local automatic admission and trusted feedback implementation

- Disabled-by-default Linear Todo admission with deterministic
  priority/identifier/UUID ordering and serial handoff.
- Singleton scheduling lease, reservation/mutation journal, one-active-run
  policy, durable retry schedule, worker, macOS LaunchAgent controls, and
  non-root headless LaunchDaemon supervision.
- Sanitized transport-neutral operator-attention events and queue-decision
  projection.
- Trusted I-Fan inline review feedback lifecycle, same-session repair, fresh
  review, idempotent GitHub App reply, and conversation-resolution wait.
- Exact merge-SHA source checkout synchronization and partial cleanup recovery.
- Graceful parked-run abandon with proven managed-child termination and guarded
  local/remote ownership cleanup,
  terminal residue attention, worker slot release, owned repair-push recovery,
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

### In progress: operator experience and repair-review precision

The active product roadmap is
[#89](https://github.com/ifan0927/Agent-Loop-Controller/issues/89). It keeps the
completed controller authority model intact while adding:

- repair-aware fresh review bound to exact trusted findings;
- idempotent requests for the configured trusted human reviewer;
- a loopback-first authenticated read-only operator API and monitoring Web UI;
- fixed-authority privileged worker restart without browser password handling;
- restart-safe notification delivery to a Web UI inbox and configured outbound
  channels.

The work is decomposed into bounded issues
[#90](https://github.com/ifan0927/Agent-Loop-Controller/issues/90) through
[#98](https://github.com/ifan0927/Agent-Loop-Controller/issues/98). The final
issue is the only new live full-chain acceptance; implementation children must
not absorb adjacent roadmap scope.

## Near-Term Goals

### In progress: stabilize operator ergonomics

The worker remains alive while runs are parked, status exposes the current
parked reason, and explicit authenticated recovery answers have a durable
provenance boundary separate from automatic workflow evidence. Issues
[#92](https://github.com/ifan0927/Agent-Loop-Controller/issues/92),
[#93](https://github.com/ifan0927/Agent-Loop-Controller/issues/93),
[#95](https://github.com/ifan0927/Agent-Loop-Controller/issues/95), and
[#96](https://github.com/ifan0927/Agent-Loop-Controller/issues/96) add the
operator API, monitoring UI, fixed privileged helper, and typed restart flow
without exposing arbitrary state or root mutation.

### In progress: notification delivery

Issues [#94](https://github.com/ifan0927/Agent-Loop-Controller/issues/94) and
[#97](https://github.com/ifan0927/Agent-Loop-Controller/issues/97) deliver the
current versioned operator events beyond SQLite. Transport remains idempotent,
sanitized, and subordinate to controller state; delivery acknowledgement never
becomes workflow authority.

### Planned: Hermes application integration

Connect Hermes as the conversation, trigger, status, and notification surface.
Hermes should submit a normalized authenticated admission intent, show the same
sanitized run projection, and route structured decisions. It must not execute
Mac shell commands, read worktrees, approve GitHub reviews, resolve human
threads, or own controller state.

### Planned: broader multi-repository operation

Configuration already supports multiple repository profiles while each run
selects exactly one. The next product step is safe scheduling and operator
visibility across more repositories, with per-profile credentials, verifier
policy, and authority isolation. Cross-repository transactions and one issue
spanning multiple PRs remain outside this goal until explicitly designed.

## Longer-Term Direction

### Exploratory: authenticated API boundary

The existing transport-neutral application services and sanitized query results
can support a local authenticated API for Hermes and Web UI. Before adding an
HTTP server, define authentication/session ownership, CSRF/origin policy,
request idempotency, streaming/polling bounds, credential-safe projections, and
which recovery commands are safe to expose. The API must be an adapter over the
controller, not a second workflow engine.

### Exploratory: event-driven admission

Linear webhooks or another event source may reduce polling latency after the
same admission eligibility, singleton/concurrency, signature verification, and
deduplication rules can be preserved. Event delivery must be treated as a hint
to re-read Linear, never as authoritative task content.

### Exploratory: bounded concurrency

Multiple simultaneous runs may be considered only after per-repository and
global resource limits, fairness, credential isolation, worker crash recovery,
operator attention, and external rate limits have explicit policy. The current
one-active-run design is intentional and should not be loosened incidentally.

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
[#89](https://github.com/ifan0927/Agent-Loop-Controller/issues/89), with final
acceptance tracked by
[#98](https://github.com/ifan0927/Agent-Loop-Controller/issues/98). The completed
Round 2 trackers [#21](https://github.com/ifan0927/Agent-Loop-Controller/issues/21),
[#42](https://github.com/ifan0927/Agent-Loop-Controller/issues/42), and
[#45](https://github.com/ifan0927/Agent-Loop-Controller/issues/45) retain their
historical implementation evidence. Update this roadmap when milestone meaning
changes; do not copy full issue checklists here.
