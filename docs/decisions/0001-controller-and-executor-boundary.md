# ADR 0001: Deterministic Controller with Codex Executors

## Status

Accepted for the MVP.

## Context

I-Fan needs a reusable coding delivery loop spanning Linear, Hermes, Mac Codex,
GitHub, CodeRabbit, and a final human approval. Official Codex integrations can
run coding tasks, but do not define all I-Fan-specific admission, review,
decision, merge, and cleanup policies.

## Decision

Build a deterministic Go controller. Treat Codex as a nondeterministic executor
behind explicit implementation, resume, and fresh-review contracts.

The primary MVP executor is Mac Codex CLI. Implementation sessions are resumable.
Review sessions are always fresh, ephemeral, read-only, and independent. The MVP
uses a structured general `codex exec` review run rather than the CLI 0.144.1
built-in review subcommand. Controller-owned evidence and exact Git head SHAs
govern progression.

Linear remains task source of truth, GitHub remains code/CI/merge source of truth,
Hermes remains planning and notification interface, CodeRabbit is the second
automated PR reviewer, and I-Fan remains the final approval gate.

## Consequences

- Workflow behavior remains portable across future trigger and executor adapters.
- Local development dependencies can be used without recreating them in a cloud
  environment during the MVP.
- The project must maintain durable state, idempotency, crash recovery, CLI
  compatibility tests, and external-system reconciliation.
- The project must not grow into a replacement coding agent or general workflow
  platform during the MVP.
