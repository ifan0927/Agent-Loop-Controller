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

The direct read-only GitHub slice adds SQLite v6 sanitized observations, GitHub
App JWT and installation-token authentication, and direct REST/GraphQL evidence
collection without `gh` or user authentication. Real access is opt-in; see
[the operator handoff](docs/github-app-operator.md).

When a run is awaiting final human approval, reconciliation accepts only an
exact-HEAD `APPROVED` pull-request review from a configured immutable GitHub
`User` identity. The controller records source and observation timestamps for
approval, dismissal, changes-requested, stale-head, and rejected lookalike
evidence; it never treats a login or App/Bot identity as human approval.

The `controller start` command composes the direct Linear reader with durable
admission. It accepts one explicit IFAN identifier and complete requester
identity, re-fetches the authoritative source, enforces coding-ready eligibility,
matches exactly one controller-configured repository label, freezes the task and
repository policy, and creates or resumes one durable run. A changed source,
repository binding, or Linear branch never mutates an existing run: the run is
halted for a human decision instead.

`controller continue`, `controller open-pr`, and `controller reconcile` are the matching manual
restart entrypoints. All require the persisted run ID, requester identity,
repository, expected state, and idempotency key. They re-read Linear before
acting, reject source drift, and derive one action from durable state. Local
execution uses the existing controller; GitHub reconciliation is read-only and
requires persisted PR identity and exact candidate HEAD. States whose next step
would write a branch, PR, or merge stop explicitly until their later lifecycle
issue is implemented.

## Controller configuration

Production commands use one versioned controller composition file. It contains
the controller database and Codex settings, an inline Linear read profile, a
strict repository registry reference, and one GitHub App profile for each
registered repository. Credentials remain external: the Linear profile names a
credential source and a GitHub profile names a private-key file; neither value
is emitted by inspection output.

Validate or inspect this composition before starting a run:

```sh
go run ./cmd/ifan-loop config validate --config /absolute/path/controller.json
go run ./cmd/ifan-loop config inspect --config /absolute/path/controller.json
```

Both commands are offline. They do not read GitHub CLI authentication, user
tokens, environment credentials, database contents, or private-key contents,
and they do not write files. They validate canonical non-symlink paths and the
registry-to-GitHub identity bindings, then emit only stable profile IDs and
digests. Linear admission and GitHub reconciliation use the same loader:

```sh
go run ./cmd/ifan-loop controller start IFAN-42 \
  --config /absolute/path/controller.json \
  --requester ifan0927 --requester-database-id 123 \
  --requester-node-id <github-node-id> --requester-type User
```

After reading the previous sanitized status result, an operator may resume or
perform the read-only external reconciliation with the same compare-and-swap
evidence:

```sh
go run ./cmd/ifan-loop controller continue <run-id> \
  --config /absolute/path/controller.json \
  --requester ifan0927 --requester-database-id 123 \
  --requester-node-id <github-node-id> --requester-type User \
  --repository owner/repo --expected-state executing --idempotency-key <key>

go run ./cmd/ifan-loop controller reconcile <run-id> \
  --config /absolute/path/controller.json \
  --requester ifan0927 --requester-database-id 123 \
  --requester-node-id <github-node-id> --requester-type User \
  --repository owner/repo --expected-state pr_open --idempotency-key <key>
```

The controller configuration is strict JSON with this shape:

```json
{
  "version": 1,
  "controller": {
    "database_path": "/absolute/path/controller.db",
    "codex_binary": "codex",
    "run_timeout": "30m"
  },
  "linear": { "...": "the existing strict Linear read profile fields" },
  "repository_registry_file": "/absolute/path/repository-registry.json",
  "github_app_profiles": [
    { "id": "github-app-profile:example", "config": { "...": "the existing GitHub App profile fields" } }
  ]
}
```

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
  --db "$lab/controller.db"
```

Inspect or continue the persisted run after restarting the process:

```sh
go run ./cmd/ifan-loop local status <run-id> --db "$lab/controller.db"
go run ./cmd/ifan-loop local inspect <run-id> --db "$lab/controller.db"
go run ./cmd/ifan-loop local continue <run-id> \
  --db "$lab/controller.db" \
  --registry "$lab/repository-registry.json" \
  --decision "$lab/decision.json"
```

The two real-model Phase 1B smoke paths are opt-in and retain their disposable
labs for inspection:

```sh
./scripts/live-local-durable.sh
./scripts/live-local-resume.sh
```

The versioned local repository registry binds canonical owner/name, local roots,
base branch, verifier registry and allowed verifier IDs, immutable GitHub App and
installation identity, immutable repository ID, and trusted operator identities.
It contains references and non-secret identity only; executable verifier
commands remain compiled controller policy. A run freezes canonical sanitized
profile evidence for the selected repository; `local continue` rejects material
profile or local-ownership drift without treating unrelated registry edits or
credential rotation with unchanged identity as authority changes.

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
