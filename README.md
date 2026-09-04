# Agent Loop Controller

> **Archived v1 reference implementation.** ALC v1 completed its Phase 1
> research and engineering objective and is no longer under active feature
> development. The source, tests, evidence contracts, and operational history
> remain available for study. See [V1 Archive](docs/V1_ARCHIVE.md).

## Overview

Agent Loop Controller is a deterministic, human-gated software-delivery control
plane. It translates one coding-ready Linear issue into an isolated, resumable
Codex delivery run and records the state and evidence required to publish,
review, merge, reconcile, and clean up that run safely.

The controller is not an LLM agent. Codex reasons about and changes code; the
controller decides whether authoritative evidence permits the next workflow
transition.

## Why This Exists

A coding agent can implement a task, but it cannot by natural-language claim
prove that the task was authoritative, tests ran against the current commit,
an independent review passed, GitHub approved the same head, or an interrupted
external write completed exactly once. This project makes those concerns
durable, explicit, restart-safe, and inspectable.

## System Roles

| System | Responsibility |
| --- | --- |
| Linear | Task definition, priority, current-cycle eligibility, acceptance criteria, and the controller-owned branch name |
| Codex | Resumable implementation and repair, plus fresh independent read-only review |
| GitHub | Repository, pull request, required CI, human review, protected merge, and merge evidence |
| Hermes | Conversation, future trigger, notification, and status interface; it is not yet connected to the runtime |
| Controller | Durable state, authority snapshots, evidence, orchestration, retries, reconciliation, and owned cleanup |

The configured human operator remains the final approval authority. The
controller never approves its own work or resolves a human review conversation.

## End-to-End Workflow

```text
eligible Linear Todo
  -> reserve and move to In Progress
  -> freeze task and repository authority
  -> create an isolated worktree
  -> Codex implementation or same-session repair
  -> repository verification bound to candidate HEAD
  -> fresh independent Codex review bound to the same HEAD
  -> push one owned branch and open/adopt one owned PR
  -> observe required CI and trusted human-review feedback
  -> repair, re-verify, re-review, and reply when changes are requested
  -> wait for conversation resolution and exact-HEAD human approval
  -> guarded squash merge
  -> observe Linear completion
  -> fast-forward a safe source checkout and clean owned resources
  -> completed
```

Every code-changing repair invalidates prior verification, review, CI, and
approval evidence. Pending CI, human approval, review resolution, and Linear
completion are normal polling conditions, not reasons to manually step through
the state machine.

## Current Capabilities

- Versioned, secret-free local configuration with inline repository profiles
  and narrow GitHub App authorities, including one complete immutable
  controller-operator identity distinct from automatic admission.
- Manual Linear admission and disabled-by-default automatic Todo admission.
- Controller-owned immutable configuration generations with one-time baseline
  adoption, private bounded raw evidence, generation/digest CAS transactions,
  crash reconciliation, desired/effective runtime convergence, and fail-closed
  new-admission fencing.
- One Controller-wide durable typed configuration draft for routine timeout and
  automatic-admission settings, with revision CAS, sanitized validation,
  semantic preview, replayable apply through the generation service, and
  explicit discard. Eligible retained generations can be projected into a
  source-bound forward-rollback draft without restoring historical private
  authority or creating another apply path.
- Explicit guarded inline configuration migration from readable schemas 2-4
  to schema 5, with sanitized semantic preview, exact convergence and CAS
  authority, durable `schema_migration` provenance, response-loss replay, and
  operator-owned worker restart.
- Durable per-repository `enabled`/`disabled` lifecycle authority with immutable
  incarnation identity, one-time compatibility-preserving baseline adoption,
  eight-dimension read-only readiness snapshots, restart-safe recheck receipts,
  guarded draft/preview/apply retirement, and transaction-time admission
  fencing for both manual and automatic starts. Retirement removes only the
  exact configuration profile after fresh worker convergence and preserves run,
  receipt, audit, local, GitHub, and Linear history.
- Restart-safe existing-checkout onboarding with typed open, read-only
  preflight, semantic preview, start, cancel, show, and resume commands. The
  persisted worker saga creates only Controller-owned roots, creates or adopts
  the exact Linear repository label, applies one source-bound configuration
  addition, waits for fresh worker convergence, publishes readiness, and stops
  in a disabled lifecycle until an operator explicitly enables it.
- Deterministically ordered bounded worker scheduling with one nonterminal run
  per repository, a configurable local-heavy-work capacity, durable leases and
  permits, restart-stable parked states, retry schedules, local
  operator-attention records, and durable provenance for explicit authenticated
  recovery answers.
- Isolated worktrees, resumable Codex implementation sessions, structured
  outcomes, repository-owned verifier commands, and fresh read-only review that
  binds post-repair passes to exact controller-selected finding dispositions.
- Exact-HEAD branch push, owned PR creation/adoption, required-check and review
  reconciliation, trusted inline feedback repair, and idempotent App replies.
- Exact-HEAD human approval, guarded squash merge, Linear completion
  observation, safe source-checkout synchronization, and ownership-checked
  cleanup.
- Requester-authorized status/inspection and narrow recovery actions for
  exhausted typed retries, interrupted delivery, graceful parked-run
  abandonment with proven managed-child termination, residue attention, and
  verified external merges.
- Presentation-independent controller, repository, frozen-run, and onboarding
  authorization scopes; scope-aware run and scheduler collections filter before
  counting, ordering, and pagination, and detail reads do not distinguish an
  unknown target from an unauthorized one.
- Persisted-state legal-action offers for decision, retry, abandon, owned-push,
  and external-merge recovery, backed by a scope-neutral operation
  receipt lifecycle that survives reconnects and controller restarts.
- Automatic Controller-wide SQLite audit-integrity observations over a closed
  seven-family registry, with transactionally enforced source generations,
  restart-safe bounded scanning, mutation-fenced publication, and authorized
  sanitized summary and affected-scope application queries. The configured
  operator can request or exactly replay one receipt-backed explicit recheck;
  it scans only the post-receipt generation through the same worker lane and
  atomically binds the settled receipt, generic activity event, and immutable
  observation.
- macOS LaunchAgent and headless system LaunchDaemon tooling for building,
  installing, validating, starting, observing, and stopping exactly one local
  non-root supervisor process.

## Safety and Trust Model

- External issue, comment, and API text is untrusted data, never a shell
  command or authority by assertion.
- Task, repository profile, verifier policy, requester, branch, and external
  identities are frozen or revalidated before use.
- Verification, fresh review, checks, approval, merge, and cleanup evidence is
  bound to exact Git SHAs.
- External writes follow persisted intent, bounded execution, observation, and
  idempotent reconciliation.
- Controller-managed processes use explicit argv and restricted environments;
  controller-managed Codex runs ignore global user configuration.
- Credentials remain outside configuration snapshots, SQLite projections,
  artifacts, logs, and documentation.
- A process or host restart resumes from SQLite and observed external state; it
  never treats an interrupted response as success.

## Quick Start

Prerequisites are Go from [`go.mod`](go.mod), Git, a compatible authenticated
Codex CLI, Linear access, and a selected-repository GitHub App. Production
configuration is macOS-local by default.

`agentctl` is the canonical executable and `cmd/agentctl` is its command
package. New LaunchAgent and LaunchDaemon installations use the neutral
`io.agent-loop-controller.worker` label. Existing `ifan-loop` or
`com.ifan.agent-loop-controller.worker` installations must use the bounded
migration and rollback procedure in [Operations](docs/operations.md); never
bootstrap a neutral service beside a legacy worker.

```sh
mkdir -p ./bin
go build -o ./bin/agentctl ./cmd/agentctl
./bin/agentctl config init
# Edit the generated secret-free controller.json and provision credentials
# outside the repository.
./bin/agentctl config validate
./bin/agentctl config inspect
./bin/agentctl config doctor
./bin/agentctl controller worker --once
./bin/agentctl operator
```

`config init` deliberately creates an incomplete starter. Follow
[Operations](docs/operations.md) before enabling automatic admission or any
GitHub write capability.

## Normal Operator Flow

For the supported automatic path, validate configuration and credentials,
enable the bounded Linear Todo admission policy, then run
`agentctl controller worker` directly, under the per-login LaunchAgent, or
under the system LaunchDaemon for pre-login headless recovery.
The normal worker runs until SIGINT/SIGTERM rather than expiring on a global
timer; durable recovery and operation-specific timeouts remain authoritative.
Observe a run with `controller status` or `controller inspect`. If the run stops
at `awaiting_human_decision`, use `agentctl operator` for the normal local flow;
it settles the decision locally and the running worker resumes it on the next
cycle. The recovery-only `controller continue --decision ...` path instead
owns and performs its resume before returning. Human review resolution and
approval happen in GitHub; the driver observes them and continues
automatically.

For the routine local operator, run `agentctl operator`. It authenticates as
the configured Controller operator, opens only the already-bound current
configuration authority, and provides Overview, filtered cursor-paginated Runs,
a complete cursor-paginated read-only Attention inbox, a shared compact Run
detail, and a cursor-paginated Repositories destination with shared Repository
detail. Run detail provides the bounded human-decision request, fixed-option
selection, optional bounded instructions, exact review, confirmation, settled
receipt, and worker-resume observation for a current `decide` offer. Repository
detail shows the Controller-owned acceptance conclusion and
all eight readiness dimensions; a currently authorized ready-disabled
repository can be explicitly confirmed and enabled through the existing
receipt-backed service. No other TUI mutation is implemented. The command
accepts no requester override flags and does not start or supervise the worker.

Low-level `continue`, `push`, `open-pr`, `reconcile`, `merge`,
`reconcile-linear`, and `cleanup` commands are recovery interfaces, not the
normal workflow.

## Project Status

ALC v1 is a completed Phase 1 reference implementation. Its production MVP,
automatic admission, exact-head evidence, independent review, bounded repair,
human authority, restart recovery, configuration convergence, and isolated
live-acceptance milestones are complete. Development stopped after the core
coding-delivery loop and its operational vertical slice had been proven; this
was a product-boundary decision, not a failed implementation.

The partial local TUI remains in the repository as historical implementation
evidence. Overview, Runs, Attention, shared Run detail, Repositories, shared
Repository detail, the R07 decision flow, and R08 repository enablement were
implemented. Queue, Onboarding, Settings, System/Audit, disable/recheck/removal,
and other planned operator mutations were intentionally not completed. The
former active umbrella
[#99](https://github.com/ifan0927/Agent-Loop-Controller/issues/99) is retained as
historical planning evidence and is superseded by the archival decision.

See [Roadmap](docs/roadmap.md) for completed milestones and the archival
disposition of former plans.

## Documentation

- [Architecture](docs/architecture.md): components, domain invariants, state
  machine, persistence, authority, and recovery design.
- [Operations](docs/operations.md): installation, configuration, every
  human-facing command, normal flow, recovery, supervision, and troubleshooting.
- [Development](docs/development.md): repository layout, tests, fixtures, E2E,
  migrations, extension rules, and contribution checks.
- [Roadmap](docs/roadmap.md): product direction, completed milestones, archival
  disposition, and historical deferred directions.
- [V1 Archive](docs/V1_ARCHIVE.md): final status, proven behavior, intentionally
  unfinished areas, and the highest-value references for future work.
- [GitHub App runbook](docs/runbooks/github-app.md) and
  [live-E2E runbook](docs/runbooks/live-e2e.md): high-risk credential/permission
  setup and destructive isolated acceptance procedures.
- [ADR 0001](docs/decisions/0001-controller-and-executor-boundary.md): accepted
  controller/executor boundary.
