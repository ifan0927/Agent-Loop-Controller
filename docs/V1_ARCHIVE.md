# ALC v1 Archive

## Status

Agent Loop Controller v1 is a completed Phase 1 reference implementation. It is
no longer an active feature-development product or an active local runtime.

Its primary research and engineering goal was to demonstrate that a
deterministic, human-governed AI coding-delivery loop could operate safely from
admission through implementation, evidence, independent review, bounded repair,
human approval, delivery reconciliation, and cleanup. The repository preserves
that result for inspection, verification, and selective reference by future
work.

Archival does not mean that every proposed operator-product feature was
finished. It means that the core hypothesis and the complete vertical slice had
been proven far enough that continuing to expand the v1 platform boundary was
no longer the right investment.

## What v1 Proved

The repository implementation, deterministic fixtures, and isolated live
acceptance demonstrated:

- a deterministic Controller around a nondeterministic coding executor;
- immutable admitted task and repository-policy snapshots;
- Controller-owned isolated worktrees and Candidate commits;
- resumable implementation sessions;
- repository verification bound to the exact Candidate HEAD;
- fresh, independent, read-only review bound to that same HEAD;
- structured review outcomes, findings, and dispositions;
- repair against Controller-selected findings, followed by a new Candidate and
  invalidation of prior evidence;
- bounded repair, durable human-decision gates, and manual escalation;
- durable state, leases, retries, idempotency, side-effect intent, observation,
  reconciliation, and restart recovery;
- configuration generations, convergence, drift detection, and admission
  fencing;
- bounded multi-repository scheduling and restart-safe repository onboarding;
- Linear admission and lifecycle observation plus GitHub branch, pull request,
  CI, review, approval, protected merge, source synchronization, and owned
  cleanup integration; and
- non-root macOS worker supervision, including headless LaunchDaemon operation.

The accepted live milestone described in [Roadmap](roadmap.md) is the authority
for which external end-to-end behaviors were exercised. Deterministic tests and
fixtures remain the reproducible verification authority for the repository.

## Why Active Development Stopped

ALC v1 succeeded as a vertical slice, but that proof caused one repository and
runtime to accumulate several distinct product responsibilities:

- task-system admission and lifecycle integration;
- scheduling, worker supervision, and local operational recovery;
- provider-specific Codex orchestration;
- GitHub delivery and merge lifecycle;
- configuration authority and repository onboarding; and
- a local operator platform and partial TUI.

The core coding-loop semantics remain valuable. The ownership boundary around
them became too broad. Future ALC architecture may use this repository as
evidence, but it should not inherit every v1 platform responsibility by default.
This archive deliberately does not specify or implement a complete v2 design.

## High-Value References

Start with these stable documents and implementation seams:

- [Architecture](architecture.md) for authorities, state, exact-head evidence,
  security invariants, and restart behavior.
- [ADR 0001](decisions/0001-controller-and-executor-boundary.md) for the
  deterministic Controller and nondeterministic executor boundary.
- [Development](development.md) and
  [`scripts/verify-controller.sh`](../scripts/verify-controller.sh) for the
  canonical gate and test strategy.
- [Live E2E runbook](runbooks/live-e2e.md),
  [`scripts/live-post-approval-dogfood.sh`](../scripts/live-post-approval-dogfood.sh),
  and [`testdata/continuous-supervisor-fixture-summary.json`](../testdata/continuous-supervisor-fixture-summary.json)
  for external and restart-safety acceptance evidence.
- [`internal/domain`](../internal/domain) for task, outcome, finding, approval,
  and state contracts.
- [`internal/application/local_controller.go`](../internal/application/local_controller.go)
  for Candidate creation, exact-head verification, fresh review, and repair
  orchestration.
- [`internal/application/repair.go`](../internal/application/repair.go),
  [`internal/application/delivery.go`](../internal/application/delivery.go), and
  their tests for finding normalization, repair selection, and delivery gates.
- [`internal/adapters/sqlite`](../internal/adapters/sqlite) for durable state,
  migration, idempotency, intent/observation, and restart recovery patterns.
- [`internal/application/configuration_service.go`](../internal/application/configuration_service.go)
  and [`internal/adapters/configuration`](../internal/adapters/configuration)
  for configuration convergence and filesystem authority patterns.
- [`internal/adapters/git`](../internal/adapters/git) and
  [`internal/adapters/codex`](../internal/adapters/codex) for worktree, Candidate,
  process, and provider-specific command boundaries.

## Known Incomplete and Intentionally Unfinished Areas

The local operator TUI is intentionally partial. The implemented reference
surface includes Overview, Runs, Attention, shared Run detail, Repositories,
shared Repository detail, the R07 human-decision flow, and R08 repository
enablement. The following former roadmap items are not scheduled for completion
in v1:

- Queue, Onboarding, Settings, and System/Audit destinations;
- repository disable, recheck, removal, and onboarding in the TUI;
- remaining run and Attention mutations;
- the 10/10 routine-operator scenario target;
- outbound notification delivery and Hermes integration;
- HTTP, Web UI, webhook admission, and other speculative adapters.

The code and issue history for these areas are retained. Their presence is not
an instruction to resume the former roadmap.

## Reference Verification and Runtime Use

The canonical reference verification remains:

```sh
./scripts/verify-controller.sh
```

The operations documentation remains historical and reproducibility material.
Do not treat its installation or bootstrap procedures as an instruction to
reactivate an archived v1 runtime. Any exceptional reproduction should use an
isolated disposable repository and credentials, follow the documented safety
boundaries, and be explicitly authorized.

The archival Git reference should be tagged `v1-reference` after the archival
change is merged into the protected default branch. A host-specific runtime
inventory, sanitized SQLite/evidence archive location, and shutdown result are
recorded in the archival session report rather than committed to this public
repository.
