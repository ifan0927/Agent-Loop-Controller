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

I-Fan remains the final human authority. The controller never approves its own
work or resolves a human review conversation.

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
  -> observe required CI and trusted I-Fan review feedback
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
  and narrow GitHub App authorities.
- Manual Linear admission and disabled-by-default automatic Todo admission.
- Priority-only single-run worker scheduling, durable leases, retry schedules,
  restart-stable parked states, local operator-attention records, and durable
  provenance for explicit authenticated recovery answers.
- Isolated worktrees, resumable Codex implementation sessions, structured
  outcomes, repository-owned verifier commands, and fresh read-only review.
- Exact-HEAD branch push, owned PR creation/adoption, required-check and review
  reconciliation, trusted inline feedback repair, and idempotent App replies.
- Exact-HEAD human approval, guarded squash merge, Linear completion
  observation, safe source-checkout synchronization, and ownership-checked
  cleanup.
- Requester-authorized status/inspection and narrow recovery actions for
  interrupted delivery, abandoned pre-delivery runs, and verified external
  merges.
- macOS LaunchAgent tooling for building, installing, validating, starting,
  observing, and stopping one local worker.

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

```sh
mkdir -p ./bin
go build -o ./bin/ifan-loop ./cmd/ifan-loop
./bin/ifan-loop config init
# Edit the generated secret-free controller.json and provision credentials
# outside the repository.
./bin/ifan-loop config validate
./bin/ifan-loop config inspect
./bin/ifan-loop config doctor
./bin/ifan-loop controller worker --once
```

`config init` deliberately creates an incomplete starter. Follow
[Operations](docs/operations.md) before enabling automatic admission or any
GitHub write capability.

## Normal Operator Flow

For the supported automatic path, validate configuration and credentials,
enable the bounded Linear Todo admission policy, then run
`ifan-loop controller worker` directly or under the provided LaunchAgent.
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
and recovery implementation slices are complete. The current stabilization
gate is a second isolated live E2E that proves the entire automatic path after
the runtime gaps discovered during the first attempt were remediated. Hermes
runtime integration, a human-facing Web UI, real notification delivery, public
API/webhook admission, and broader concurrent/multi-repository operation remain
planned or exploratory rather than implemented.

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
