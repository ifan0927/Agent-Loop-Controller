# Agent Loop Controller Project Profile

This approved, project-owned Profile records stable context and routes design
topics to canonical authorities. It does not copy governance, issue or roadmap
state, session history, active questions, temporary assumptions, or detailed
architecture.

## Project identity

- Project: Agent Loop Controller (ALC)
- Goal: Provide deterministic, human-gated control around isolated Codex
  software-delivery executions.
- Domain: Local-first software-delivery orchestration and evidence governance.
- Repository: `ifan0927/Agent-Loop-Controller`
- Profile path: `.issue-spec/project-profile.md`
- Profile status: Approved
- Last approved: 2026-08-21

## Project boundaries

- Product runtime: ALC consumes coding-ready Linear tasks for configured target
  repositories and owns deterministic workflow state and evidence.
- Repository development: ALC development is GitHub-issue-first and performed
  only in a manually launched Codex session; ALC never develops itself.
- Design methodology: A manual design session may locate Issue Spec Studio via
  `ISSUE_SPEC_STUDIO_PATH`. Studio remains read-only and outside ALC runtime.

Load [Architecture](../docs/architecture.md) when a task needs the complete
product boundary, authority model, component responsibilities, or runtime
invariants. Load [Roadmap](../docs/roadmap.md) for current direction and
explicit non-goals.

## Stable constraints and invariants

- The Controller is a deterministic state machine; Codex is a nondeterministic
  executor whose claims require Controller-owned evidence.
- Linear is the runtime task authority. Git, GitHub, tests, CI, configured human
  approval, and Controller persistence retain their documented distinct
  authorities.
- Controller-managed executions use isolated resources and exact-head evidence;
  a code change invalidates earlier verification and review.
- Domain and application packages remain independent from CLI, SQLite, HTTP,
  filesystem, Git, Linear, GitHub, and concrete Codex process details.
- The product is single-user and local-first. Future adapters must reuse typed
  Controller-owned application contracts without creating another state
  machine.

These are navigation summaries only. [Architecture](../docs/architecture.md)
and accepted ADRs remain authoritative.

## Development workflow

- Primary development flow: GitHub issue, then user-launched Codex, then the
  issue-authorized repository delivery flow. Never route ALC development
  through ALC.
- Verification entry point: `./scripts/verify-controller.sh`
- Delivery authority: GitHub repository policy and the selected GitHub issue.
- Development authority: [AGENTS.md](../AGENTS.md) and
  [Development](../docs/development.md).

## Design routing

- Studio bootstrap: `$ISSUE_SPEC_STUDIO_PATH/START_HERE.md`
- Active checkpoint: `.issue-spec/active.md` (optional; missing means none)
- Project-owned checkpoint template: [active.template.md](active.template.md)
- Issue authority: GitHub Issues in `ifan0927/Agent-Loop-Controller`
- Roadmap authority: [docs/roadmap.md](../docs/roadmap.md) for product direction
  and milestones; GitHub Issues for published implementation slices.

The environment variable is manual development context only. It is not ALC
configuration, a runtime prerequisite, an environment-forwarding contract, or
an instruction to load Studio for implementation-ready work.

## Repository map

| Area | Ref | Purpose | Load when | Authority |
| --- | --- | --- | --- | --- |
| Root routing | [AGENTS.md](../AGENTS.md) | Select implementation or design context and preserve safety | Every task | Canonical |
| Project entry | [README.md](../README.md) | Product value, current capability, quick start | Orienting to the project or current capability | Canonical |
| Architecture | [docs/architecture.md](../docs/architecture.md) | System boundaries, modules, state, authority, security | Changing behavior, module boundaries, runtime integrations, or evidence | Canonical |
| Operations | [docs/operations.md](../docs/operations.md) | Configuration, commands, supervision, recovery | Changing operator-facing behavior or procedures | Canonical |
| Development | [docs/development.md](../docs/development.md) | Workflow, tests, fixtures, migrations, contribution and design context | Planning or verifying repository changes | Canonical |
| Roadmap | [docs/roadmap.md](../docs/roadmap.md) | Direction, milestone meaning, explicit non-goals | Changing product scope, sequencing, or capability status | Canonical |
| CI | [.github/workflows/ci.yml](../.github/workflows/ci.yml) | Hosted verification entrypoint | Changing delivery gates or CI | Supporting evidence |
| Verification | [scripts/verify-controller.sh](../scripts/verify-controller.sh) | Canonical local and CI gate | Before delivery | Canonical command |

## Domain and architecture routing

| Topic | Ref | Purpose | Load when | Authority |
| --- | --- | --- | --- | --- |
| Controller/executor boundary | [ADR 0001](../docs/decisions/0001-controller-and-executor-boundary.md) | Accepted deterministic Controller and Codex executor split | Reconsidering execution authority | Accepted decision |
| State, evidence, authorization | [Architecture](../docs/architecture.md) | Legal transitions, exact-head evidence, human authority | Changing workflow policy or durable evidence | Canonical |
| Local operator interface | [Architecture](../docs/architecture.md#planned-local-operator-interface-boundary) | Typed application and presentation boundaries | Designing CLI, TUI, or future adapters | Canonical |

## Risk governance routing

| Topic | Ref | Purpose | Load when | Authority |
| --- | --- | --- | --- | --- |
| Security | [Architecture](../docs/architecture.md#13-security-invariants) | External-input, credential, process, and authority invariants | Authentication, authorization, subprocess, prompt, artifact, or sensitive-data work | Canonical |
| Migration | [Development](../docs/development.md#database-migrations) | Forward-only SQLite migration practice | Persistent schema or data changes | Canonical |
| Compatibility | [Operations](../docs/operations.md) | Supported commands, configuration, and legacy migration behavior | Public CLI or operator contract changes | Canonical |
| External side effects | [Development](../docs/development.md#adding-a-new-side-effect) | Typed ports, intent, reconciliation, and tests | Adding or changing an external write | Canonical |
| Live acceptance | [Live E2E runbook](../docs/runbooks/live-e2e.md) | Isolated credentialed acceptance procedure | A change explicitly requires live external verification | Canonical runbook |

## Stable terminology

| Term | Meaning | Authority |
| --- | --- | --- |
| Controller | Deterministic owner of workflow state, policy, and evidence | [Architecture](../docs/architecture.md) |
| Codex executor | Nondeterministic implementation, repair, or fresh-review process | [ADR 0001](../docs/decisions/0001-controller-and-executor-boundary.md) |
| Project Profile | Stable project facts and topic-to-authority routing | This file |
| Active checkpoint | Optional temporary resumable state for one unresolved design discussion | [Development](../docs/development.md#active-design-checkpoint-lifecycle) |

## Approval

- Approved by: Repository owner through GitHub issue #116 authorization
- Approved at: 2026-08-21
- Notes: Initial prospective adoption; existing issues and roadmap remain in
  their current authorities.
