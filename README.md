# Agent Loop Controller

Agent Loop Controller is I-Fan's deterministic control plane for turning a
coding-ready Linear issue into a Codex-driven, human-gated pull request.

The controller does not replace Codex, Linear, GitHub, Hermes, Hindsight, or
CodeRabbit. It coordinates them through explicit contracts and durable state.

## Intended delivery loop

```text
Trigger
  -> Linear issue snapshot
  -> isolated worktree
  -> Codex implementation session
  -> controller verification
  -> fresh independent Codex review
  -> repair and re-review when needed
  -> pull request
  -> CodeRabbit review
  -> repair, verification, and fresh internal re-review when needed
  -> I-Fan final approval
  -> squash merge and cleanup
```

A pull request must not be opened until the internal fresh review passes. Any
change to the reviewed head invalidates the review.

## Current foundation

This initial layout defines:

- trigger, task, policy, outcome, and review contracts;
- deterministic lifecycle states and allowed transitions;
- Codex implementation, resume, and fresh-review command specifications;
- versioned JSON schemas for implementation and review outcomes;
- a `plan` command that validates a task snapshot and renders an execution plan;
- MVP, architecture, roadmap, and Hermes handoff documentation.

Phase 1A also provides an experimental local executor spike for disposable
fixture repositories. It materializes isolated artifacts, preflights the
installed Codex CLI, runs structured implementation and fresh-review sessions,
executes controller-owned verification, creates a local candidate commit, and
stops at an approval-ready simulation. It does not call Linear, push a branch,
open a pull request, or write durable run state.

Phase 1B adds an experimental local durable controller trial. It admits a
simulated Linear issue JSON, freezes the normalized task and local repository
registry snapshots, provisions a controller-owned dedicated worktree, and uses
SQLite as the authoritative run and transition state. Implementation, explicit
session resume, verification, candidate commit, and fresh review evidence survive
controller restarts. A renewable SQLite lease prevents competing local controller
processes from operating one run concurrently, and artifact digests protect
reused verification/review evidence. A successful trial stops at
`approval_ready` without push or pull request creation.

The durable controller owns model selection: implementation and explicit resume
use `gpt-5.6-terra`, while fresh independent review uses `gpt-5.6-sol`. SQLite
persists both run policy values and each attempt's requested model; legacy runs
without this evidence are not resumed.

The post-approval delivery slice adds SQLite v5 evidence and explicit states for
branch push, one-PR publication, bounded CI/CodeRabbit reconciliation, repair,
I-Fan approval, squash merge, Linear completion observation, and owned cleanup.
Default integration uses fake GitHub plus a disposable local bare origin.

## Try the contract planner

```sh
mkdir -p /tmp/example-worktree /tmp/example-run
go run ./cmd/ifan-loop plan \
  --task ./examples/coding-task.json \
  --workspace /tmp/example-worktree \
  --artifacts /tmp/example-run
```

The command prints JSON describing the implementation and fresh-review process
invocations plus the embedded schema artifacts that must be materialized before
execution. Prompts are represented as stdin, not shell arguments.

The workspace and artifact directories must already exist. The planner compares
their filesystem identity and ancestor chain before producing a plan.

## Run the experimental local spike

```sh
go run ./cmd/ifan-loop spike \
  --task /absolute/path/to/fixture-task.json \
  --workspace /absolute/path/to/disposable-fixture \
  --artifacts /absolute/path/to/new-empty-attempt-directory
```

The fixture must be a clean Git repository on the task's working branch, have a
local `origin/<base_branch>` remote-tracking ref, contain no ignored workspace
files, and reference only the controller-owned `fixture-go-test` verifier. The
controller runs verification before committing, creates the candidate commit itself, then
runs verification again so approval evidence is bound to the exact candidate
HEAD. The fresh review is a new ephemeral read-only general `codex exec` run.

The real-model smoke test is deliberately opt-in and creates only temporary
local repositories:

```sh
./scripts/live-spike.sh
```

## Run the local durable trial

Create an isolated local lab containing a bare origin, source checkout,
worktree/run roots, registry, and simulated issue:

```sh
lab="$(./scripts/create-local-lab.sh)"
go run ./cmd/ifan-loop local start \
  --issue "$lab/simulated-issue.json" \
  --registry "$lab/repository-registry.json" \
  --db "$lab/controller.db" \
  --run-root "$lab/runs" \
  --worktree-root "$lab/worktrees"
```

Inspect or continue the persisted run after restarting the process:

```sh
go run ./cmd/ifan-loop local status <run-id> --db "$lab/controller.db"
go run ./cmd/ifan-loop local inspect <run-id> --db "$lab/controller.db"
go run ./cmd/ifan-loop local continue <run-id> \
  --db "$lab/controller.db" \
  --decision "$lab/decision.json"
```

The two real-model Phase 1B smoke paths are opt-in and retain their disposable
labs for inspection:

```sh
./scripts/live-local-durable.sh
./scripts/live-local-resume.sh
```

The local repository registry contains paths and allowed verifier IDs only.
Executable verifier commands remain compiled controller policy.

Post-approval destructive smoke must use a disposable local bare origin and fake
GitHub service, or an explicitly authorized isolated GitHub test repository.
Never use this repository's production remote as a dogfood target.

The opt-in full dogfood scenario starts through the public CLI, uses real Terra
implementation and fresh Sol review, restarts at the external boundary, and then
uses a bare origin plus fake GitHub evidence through merge and cleanup:

```sh
./scripts/live-post-approval-dogfood.sh
```

## Documentation

- [Architecture](docs/architecture.md)
- [MVP scope](docs/mvp.md)
- [Roadmap](docs/roadmap.md)
- [Architecture decision](docs/decisions/0001-controller-and-executor-boundary.md)
- [Hermes handoff](docs/handoff/hermes.md)
