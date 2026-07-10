# Roadmap

## Phase 0: Contract foundation

- Domain contracts and state vocabulary.
- Versioned Codex implementation and review schemas.
- Safe command specifications and prompt boundaries.
- Architecture, MVP, and handoff documentation.

## Phase 1: Executable local vertical slice

- SQLite run store and transition journal.
- Linear read adapter and IFAN admission policy.
- Repository registry and worktree manager.
- Codex subprocess runner with CLI capability preflight, version recording,
  JSONL/session capture, and cancellation.
- Repository verification runner.
- Fresh review and repair loop.
- GitHub PR publisher and human-gated finalizer.
- Crash recovery and reconciliation CLI.

## Phase 2: External trigger adapters

- Hermes explicit start command.
- Linear webhook admission.
- Cron polling as a reconciliation and optional admission source.
- Structured decision questions routed to Linear and Hermes.
- Notifications for blocked, approval-ready, and failed runs.

All adapters emit the same `TriggerSignal`; they do not bypass controller policy.

## Phase 3: Additional executors and scale

- Optional Codex Cloud executor.
- Multiple concurrent isolated runs with resource limits.
- Dedicated GitHub App identity.
- Repository-specific verifier plugins.
- Operational dashboards and cost/latency metrics.

## Reserved Phase 4: Workflow evolution

Loop 4 is deliberately not active in the MVP. Earlier phases retain structured
run traces, prompt/schema versions, failure categories, human decisions, review
findings, timing, and outcome evidence so a future analyzer can propose changes.

Any proposal to change `AGENTS.md`, skills, prompts, admission policy, verifier
rules, or controller behavior must be reviewable, versioned, and approved by a
human before application. Hindsight stores only durable approved decisions, not
raw run logs or transient failures.
