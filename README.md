# Agent Loop Controller

Agent Loop Controller is I-Fan's deterministic control plane for turning a
coding-ready Linear issue into a Codex-driven, human-gated pull request.

The controller does not replace Codex, Linear, GitHub, Hermes, or Hindsight.
It coordinates them through explicit contracts and durable state.

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
  -> required CI reconciliation
  -> repair, verification, and fresh internal re-review when required CI fails
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
branch push, one-PR publication, bounded required-CI reconciliation, repair,
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

If GitHub rejects an otherwise guarded squash merge with a policy-compatible
`405`, `409`, or `422`, the controller does one new stable read. It enters
`awaiting_github_mergeability` only when that read still proves the immutable
owned PR, exact head, passing checks, valid approval, and an unresolved thread
previously replied to by the controller. This state is read-only: it never
resolves a conversation or repeats a merge request. Once GitHub shows the
tracked thread resolved, the ordinary exact-head merge gates are read again
before one retry. Authority drift, ambiguous topology, missing reply evidence,
or any other rejection requires `manual_intervention`.

`controller run` is the normal production entrypoint. It accepts one explicit
IFAN identifier and complete requester identity, re-fetches the authoritative
source, enforces coding-ready eligibility, matches exactly one
controller-configured repository label, freezes the task and repository policy,
and creates or resumes one durable run. It then drives the run through Codex,
verification, fresh review, push, PR creation, required-CI reconciliation,
squash merge, Linear completion observation, and owned cleanup.

The durable driver, rather than an operator command sequence, owns normal
delivery progression. It may safely restart from persisted intent and evidence
at any external boundary. It pauses only for an unresolved human decision,
`manual_intervention`, a terminal outcome, or I-Fan's final approval for the
exact PR head. While awaiting final approval it observes GitHub; a valid approval
causes the same driver to continue with merge, Linear reconciliation, and
cleanup. A changed source, repository binding, or Linear branch never mutates an
existing run: it becomes a human decision point instead.

`controller drive <run-id>` resumes an already admitted run after a controller
process restart. `controller status` and `controller inspect` are read-only
observation tools. The lower-level `continue`, `recover-owned-push`, `push`, `open-pr`, `reconcile`,
`merge`, `reconcile-linear`, and `cleanup` commands remain intentional
recovery/debug interfaces for an incident or a bounded E2E fault injection;
they are not the normal operator workflow. Those recovery commands require the
persisted run ID, requester identity, repository, expected state, and
idempotency key. The key is a run-scoped compare-and-swap value, not a credential
and cannot authorize a requester by itself.

`recover-owned-push` is the one narrow exception for a repair that halted while
updating an already open controller-owned PR. It accepts only
`manual_intervention`, requires an unchanged Linear task and retained owned PR,
then returns to `approval_ready` without a Git or GitHub write. The resumed
driver still revalidates exact-HEAD evidence, the remote SHA, and its
fast-forward lease before it can update that branch.

## Controller configuration

Production commands use one versioned controller composition file. On macOS the
default location is `~/Library/Application Support/agent-loop-controller/controller.json`;
`--config /absolute/path/controller.json` remains an explicit test, CI, or
multi-environment override. Version 3 keeps the controller database and Codex
settings, an inline Linear read profile, inline repository entries, and one
GitHub App profile for each repository in that one document. Credentials remain
external: the Linear profile names a credential source and a GitHub profile
names a private-key file; neither value is emitted by inspection output.

Create the secret-free starter document once, then add the actual fixture App
and repository identities before validation:

```sh
go run ./cmd/ifan-loop config path
go run ./cmd/ifan-loop config init
```

`config init` creates the file exclusively with mode `0600` and its final
directory with mode `0700`; it never overwrites a configuration. The starter is
intentionally not runnable until an operator supplies a repository and GitHub
App profile. See [configuration](docs/configuration.md).

Validate or inspect this composition before starting a run:

```sh
go run ./cmd/ifan-loop config validate
go run ./cmd/ifan-loop config inspect
```

Both commands are offline. They do not read GitHub CLI authentication, user
tokens, environment credentials, database contents, or private-key contents,
and they do not write files. They validate canonical non-symlink paths and the
registry-to-GitHub identity bindings, then emit only stable profile IDs and
digests. Linear admission and GitHub reconciliation use the same loader.

Start the normal durable delivery loop with one explicit trigger. The process
continues through every safe transition; do not run one manual delivery command
per state during normal operation:

```sh
go run ./cmd/ifan-loop controller run IFAN-42 \
  --config /absolute/path/controller.json \
  --requester ifan0927 --requester-database-id 123 \
  --requester-node-id <github-node-id> --requester-type User
```

The default driver process runs for up to 24 hours (adjust deliberately with
`--max-runtime`, at most seven days). It writes the restart-safe run ID to
sanitized stderr before delivery polling begins. If its process is deliberately
stopped, reaches that runtime limit, or the host restarts, use `drive` to resume
the persisted run; it derives the allowed next action from SQLite rather than
accepting a hand-written state transition:

```sh
go run ./cmd/ifan-loop controller drive <run-id> \
  --config /absolute/path/controller.json \
  --requester ifan0927 --requester-database-id 123 \
  --requester-node-id <github-node-id> --requester-type User
```

Use `status` or `inspect` to observe a paused run, retain its evidence, and
identify an operator decision or a conflict. Only then should an operator use a
specific recovery command with its compare-and-swap evidence:

```sh
go run ./cmd/ifan-loop controller status <run-id> \
  --config /absolute/path/controller.json \
  --requester ifan0927 --requester-database-id 123 \
  --requester-node-id <github-node-id> --requester-type User
```

After an authoritative squash merge, the driver performs read-only Linear
completion observations. It records the immutable merge binding plus sanitized
request metadata and the observed completion state. A completed issue advances
to owned cleanup. Canceled, ambiguous, unreadable, or bounded-timeout evidence
fails closed for an operator. Cleanup revalidates each owned resource before
deleting the dedicated worktree and owned local and remote branches; artifact
directories are retained. Any mismatch, dirty worktree, or partial failure
remains auditable and can be resumed idempotently by the driver.

The version 3 controller configuration is strict JSON with this shape. The
repository policy that used to live in a separate registry file is now inline:

```json
{
  "version": 3,
  "controller": {
    "database_path": "/absolute/path/controller.db",
    "codex_binary": "codex",
    "run_timeout": "30m"
  },
  "linear": { "...": "the strict Linear read profile fields" },
  "automation": { "linear_todo_admission": { "enabled": false } },
  "github_app_profiles": [
    { "id": "github-app-profile:example", "config": { "...": "the strict GitHub App profile fields" } }
  ],
  "repositories": [
    { "owner": "example-owner", "name": "isolated-fixture", "origin_url": "git@github.com:example-owner/isolated-fixture.git", "...": "the existing repository policy fields" }
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
Its origin binding is either a local bare fixture path or a credential-free
GitHub `origin_url`; the latter must name the same `owner/name` and may use SSH
or HTTPS. It contains references and non-secret identity only; executable
verifier commands remain compiled controller policy. A run freezes canonical
sanitized profile evidence for the selected repository; `local continue` rejects
material profile or local-ownership drift without treating unrelated registry
edits or credential rotation with unchanged identity as authority changes.

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
- [Isolated external E2E dogfood](docs/e2e-dogfood.md)
- [Configuration and future UI boundary](docs/configuration.md)
- [Architecture decision](docs/decisions/0001-controller-and-executor-boundary.md)
- [Hermes handoff](docs/handoff/hermes.md)
