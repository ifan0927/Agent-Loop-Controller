# Hermes Handoff

## Repository

- GitHub: `git@github.com:ifan0927/Agent-Loop-Controller.git`
- Web: `https://github.com/ifan0927/Agent-Loop-Controller`

Hermes runs on a separate GCE host and must use GitHub as the project source. It
must not assume access to I-Fan's Mac filesystem.

## Purpose

This repository contains I-Fan's deterministic coding workflow controller. It
will translate a coding-ready Linear issue into a Mac Codex implementation,
verification, fresh independent Codex review, PR, required CI, and final human
approval loop.

## Canonical documents

- `README.md`: current project overview.
- `AGENTS.md`: durable implementation boundaries and safety rules.
- `docs/architecture.md`: contracts and component ownership.
- `docs/configuration.md`: local configuration and admission authority.
- `docs/e2e-dogfood.md`: current isolated live-E2E procedure.
- `docs/decisions/0001-controller-and-executor-boundary.md`: accepted decision.

## Hermes role

Hermes is the architecture, planning, triage, and notification interface. For the
MVP, Hermes does not execute code, access Mac worktrees, admit issues
automatically, mutate Linear state without explicit I-Fan authorization, or make
merge decisions.

Future Hermes integration will emit the same normalized `TriggerSignal` as the
worker admission path and route structured human-decision questions. Controller
admission policy remains authoritative. Hermes remains outside the #42 live E2E
and never starts, approves, resolves, or repairs a run.

## Review policy to preserve

1. Codex implementation and repository verification.
2. Fresh independent Codex review before the PR exists.
3. Required CI as the post-PR automated gate.
4. I-Fan as the final approval gate.
5. Any code-changing feedback invalidates the previous review and requires
   verification plus another fresh Codex review.

## Requested Hermes behavior

Use this repository and its canonical documents in future architecture and issue
design discussions. Treat Phase 4 workflow evolution as reserved design only;
do not propose autonomous rule or prompt mutation as an MVP capability.
