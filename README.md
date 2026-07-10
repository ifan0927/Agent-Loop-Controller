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

It intentionally does not yet execute Codex, call Linear, create worktrees, or
write durable run state.

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

## Documentation

- [Architecture](docs/architecture.md)
- [MVP scope](docs/mvp.md)
- [Roadmap](docs/roadmap.md)
- [Architecture decision](docs/decisions/0001-controller-and-executor-boundary.md)
- [Hermes handoff](docs/handoff/hermes.md)
