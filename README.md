# Agent Loop Controller

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
- Persisted-state legal-action offers for decision, retry, abandon, CI-wait,
  owned-push, and external-merge recovery, backed by a scope-neutral operation
  receipt lifecycle that survives reconnects and controller restarts.
- Automatic Controller-wide SQLite audit-integrity observations over a closed
  seven-family registry, with transactionally enforced source generations,
  restart-safe bounded scanning, mutation-fenced publication, and authorized
  sanitized summary and affected-scope application queries.
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
at `awaiting_human_decision`, submit only one of the persisted offered choices
through `controller continue --decision ...`; the running worker resumes it on
the next cycle without a separate drive command. Human review resolution and
approval happen in GitHub; the driver observes them and continues
automatically.

Low-level `continue`, `push`, `open-pr`, `reconcile`, `merge`,
`reconcile-linear`, and `cleanup` commands are recovery interfaces, not the
normal workflow.

## Project Status

The production MVP and the automatic-admission, trusted-feedback, source-sync,
recovery, headless supervision, bounded multi-repository scheduling, and second
isolated live-E2E milestones are complete. The current product focus is the
remaining Controller operator foundations followed by a local TUI operator
console in this repository under
[roadmap #99](https://github.com/ifan0927/Agent-Loop-Controller/issues/99).
Local operator identity, application authorization, operation receipts, and
legal-action offers are implemented. Activity-independent worker heartbeat,
controller-authorized runtime observation, configuration generation/CAS, and
desired/effective convergence fencing are also implemented, together with the
normal typed-change draft/preview/apply slice and Controller-owned forward
rollback, plus exact safe external-drift recovery with durable receipt replay
and no new generation. Repository lifecycle, readiness, guarded retirement,
zero-repository disabled-admission operation, admission fencing, and the
restart-safe existing-checkout onboarding saga and restart-safe empty-repository
initialization are also implemented. The latter derives a Controller-owned
source checkout, creates one deterministic empty initial revision, and uses a
guarded non-force host-SSH publication before reusing the shared
`ready_disabled` tail. Versioned routine Controller application projections
for Overview, runs and fixed delivery gates, queue, active attention,
repositories, onboarding, and settings are also implemented; they are bounded,
scope-authorized, sanitized, and side-effect free. Durable versioned activity
list/detail projections and bounded operation-receipt history are implemented
as presentation-independent application contracts. Meaningful current SQLite
facts append their immutable sanitized activity snapshots transactionally;
the worker performs bounded restart-safe reconstruction of legacy evidence and
reports explicit coverage limitations for history that was never persisted.
Unsafe or ambiguous configuration repair remains out of scope;
Controller-wide audit-integrity readiness and integrity recheck are the next
operator foundation.
The TUI and `agentctl operator` are planned, not current behavior. HTTP, a Web
UI, outbound notifications, Hermes runtime integration, public API/webhook
admission, and cross-repository transactions remain deferred or exploratory.

See [Roadmap](docs/roadmap.md) for status categories and current tracking.

## Documentation

- [Architecture](docs/architecture.md): components, domain invariants, state
  machine, persistence, authority, and recovery design.
- [Operations](docs/operations.md): installation, configuration, every
  human-facing command, normal flow, recovery, supervision, and troubleshooting.
- [Development](docs/development.md): repository layout, tests, fixtures, E2E,
  migrations, extension rules, and contribution checks.
- [Roadmap](docs/roadmap.md): product direction, completed milestones, current
  stabilization work, and longer-term goals.
- [GitHub App runbook](docs/runbooks/github-app.md) and
  [live-E2E runbook](docs/runbooks/live-e2e.md): high-risk credential/permission
  setup and destructive isolated acceptance procedures.
- [ADR 0001](docs/decisions/0001-controller-and-executor-boundary.md): accepted
  controller/executor boundary.
