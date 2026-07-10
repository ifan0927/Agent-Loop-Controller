# Roadmap

## Phase 0: Contract foundation

- Domain contracts and state vocabulary.
- Versioned Codex implementation and review schemas.
- Safe command specifications and prompt boundaries.
- Architecture, MVP, and handoff documentation.

## Phase 1: Executable local vertical slice

### Phase 1A: Local Codex executor spike

- Disposable local fixture repository only.
- Exclusive attempt artifacts and embedded structured-output schemas.
- Codex CLI capability preflight, process lifecycle, JSONL/session capture, and
  semantic outcome validation.
- Controller-owned verifier, local candidate commit, exact-HEAD fresh review,
  and approval-ready simulation.
- No Linear, durable state, push, pull request, or external approval workflow.

### Phase 1B: Local durable controller trial

- Simulated Linear issue admission and immutable task/source snapshots.
- Controller-owned local repository/verifier registry and dedicated worktree.
- Versioned SQLite run, transition, attempt, verification, review, and resource
  ownership state.
- Restart reconciliation, idempotent start/continue, and explicit-session Codex
  resume using isolated attempt artifacts.
- Exact-HEAD candidate verification, fresh structured review, and guarded local
  `approval_ready` state.
- Disposable fake-process integration plus opt-in real happy-path and resume
  smoke scripts.
- No real Linear adapter, push, pull request, CodeRabbit, merge, or cleanup.

### Later Phase 1 slices

- Linear read adapter and IFAN admission policy.
- Review-finding repair and re-review loop.
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
