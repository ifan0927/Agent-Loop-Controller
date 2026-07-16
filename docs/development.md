# Development

## Repository Layout

```text
cmd/ifan-loop/          CLI, composition root, worker, LaunchAgent, fixtures
contracts/              embedded implementation/review JSON schemas
internal/domain/        pure contracts, state topology, evidence validation
internal/application/   use cases, orchestration, policy, and ports
internal/adapters/      SQLite, process, Git, Codex, Linear, GitHub, config
scripts/                deterministic verification and disposable labs
docs/                   canonical human documentation and exceptional runbooks
.github/workflows/      CI entrypoint
```

Keep domain and application packages independent from CLI, SQL, HTTP,
filesystem, Git, Linear, GitHub, and concrete Codex process details. The CLI is
the composition adapter; it must not create an alternate workflow policy.

## Build and Local Verification

```sh
go build ./cmd/ifan-loop
gofmt -w cmd internal
go test ./...
go test -race ./...
go vet ./...
git diff --check
./scripts/verify-controller.sh
```

The canonical repository gate is `./scripts/verify-controller.sh`. It checks
formatting without rewriting, normal tests, race tests, vet, the deterministic
GitHub read fixture, and credential-pattern scanning. GitHub Actions invokes the
same script on pull requests and pushes to `main` with read-only contents
permission.

## Test Strategy

Use the narrowest layer that can prove the contract:

| Layer | Proves | Does not prove |
| --- | --- | --- |
| Domain unit | Validation and legal topology without I/O | Persistence or external behavior |
| Application unit | Authorization, state/action policy, reconciliation, and failure classification through ports | Concrete SQL, process, or HTTP behavior |
| Adapter unit/fixture | SQL transactions/migrations, argv/environment, filesystem safety, and HTTP payload/evidence mapping | Full production composition |
| CLI contract | Routing, flags, sanitized JSON/errors, and composition boundaries | Real external service policy |
| Disposable integration | SQLite + real local Git/process restart/idempotency | GitHub/Linear production behavior |
| External E2E | Selected-repository App, Linear, GitHub protection, launchd, and operator interaction | Broad production safety or concurrency |

Tests should assert authority and failure behavior, not only happy-path final
state. A process that failed to start, was interrupted, returned partial data,
or produced a mutated artifact must never be represented as successful evidence.

## Unit Tests

Package tests live beside their code. Run a focused package while iterating:

```sh
go test ./internal/domain -count=1
go test ./internal/application -count=1
go test ./internal/adapters/sqlite -count=1
go test ./cmd/ifan-loop -count=1
```

Changes to state transitions require domain topology tests plus application
evidence-gate tests. Changes to command construction require exact argv,
stdin/environment, artifact-path, and forbidden-flag coverage. Changes to JSON
contracts require schema and cross-field domain validation tests.

## Integration Tests

SQLite-backed tests should use a new temporary database and exercise real
migrations, transactions, unique constraints, CAS, and restart re-open. Git
integration tests use disposable local repositories/bare origins and the managed
process boundary. HTTP adapter tests use loopback servers and bounded versioned
fixtures; they must not contact live services.

Important integration boundaries include:

- reserve-before-create artifact/worktree ownership;
- implementation interruption and explicit session resume;
- exact-head verifier/review authorization and invalidation;
- intent-before-write plus post-interruption reconciliation;
- automatic scheduler lease, one-active-run, priority tie, and durable retry;
- trusted review feedback identity/lifecycle/reply idempotency;
- source sync and partial ownership-safe cleanup;
- CLI restart using a second process and the same SQLite database.

## Deterministic Fixtures

### GitHub App fixture

```sh
./scripts/live-github-read-fixture.sh
```

Despite its historical `live-` name, this is deterministic: it runs versioned
GitHub App REST/GraphQL fixture and CLI restart tests with no real GitHub write.
It is part of the verification gate.

### Sensitive output scan

```sh
./scripts/scan-sensitive-output.sh .
./scripts/scan-sensitive-output.sh /absolute/private/evidence-root
```

The scanner detects private-key blocks and common credential/header patterns.
It supplements code review; it is not proof that arbitrary sensitive personal
data is absent.

### Fixture delivery

`local fixture-deliver` drives post-approval behavior only against a disposable
local bare origin and fake GitHub/Linear evidence. It requires:

```sh
ifan-loop local fixture-deliver <run-id> \
  --db <controller.db> --registry <repository-registry.json> \
  --approval <explicit-fixture-approval.json>
```

The command rejects non-disposable repository topology. It is a test adapter,
not a way to provide production approval.

## Local Disposable Lab

Create an empty disposable lab containing a bare origin, source checkout,
worktree/run roots, simulated issue, and legacy fixture registry:

```sh
lab="$(./scripts/create-local-lab.sh)"
```

The helper prints the retained lab root. It writes only inside a supplied empty
directory or a newly created temporary directory.

### `plan`

Build and print a deterministic delivery plan without executing Codex:

```sh
ifan-loop plan --task <coding-task.json> \
  [--workspace <absolute-worktree>] [--artifacts <absolute-root>] \
  [--codex-binary <binary>]
```

`--task` is required. Use this for command-contract inspection, not production
admission.

### `spike`

Run the original disposable implementation/verification/commit/fresh-review
vertical slice:

```sh
ifan-loop spike --task <coding-task.json> --workspace <disposable-repo> \
  --artifacts <new-empty-directory> [--codex-binary <binary>] \
  [--timeout <duration>]
```

All three paths are required; timeout defaults to `30m`. The repository must be
disposable and already have the task's working branch. The convenience script
is:

```sh
./scripts/live-spike.sh
```

### `local start`

Run fixture admission and the durable local controller:

```sh
ifan-loop local start \
  --issue <simulated-issue.json> \
  --registry <repository-registry.json> \
  --db <controller.db> --repository <owner/name> \
  <requester flags> [--codex-binary <binary>] [--timeout <duration>]
```

All issue/registry/database/repository and complete requester flags are
required. The caller selection must match the admitted fixture issue.

The convenience script creates and retains a lab:

```sh
./scripts/live-local-durable.sh
```

### `local continue`

Resume one local run with explicit persisted authority:

```sh
ifan-loop local continue <run-id> \
  --db <controller.db> --registry <repository-registry.json> \
  --repository <owner/name> --expected-state <state> \
  --idempotency-key <key> <requester flags> \
  [--decision <decision.json>] [--codex-binary <binary>] \
  [--timeout <duration>]
```

The script below exercises an explicit decision and second-process resume:

```sh
./scripts/live-local-resume.sh
```

### `local status` / `local inspect`

Read the detailed fixture projection:

```sh
ifan-loop local status <run-id> --db <controller.db> <requester flags>
ifan-loop local inspect <run-id> --db <controller.db> <requester flags>
```

Both currently return the same detailed result. They are read-only.

### Post-approval local dogfood

```sh
./scripts/live-post-approval-dogfood.sh
```

This uses a fake explicit human approval, a second CLI process, disposable
remote branch, simulated GitHub/Linear state, and ownership cleanup. It proves
restart-safe fixture composition, not production GitHub approval.

## External E2E Dogfood

External E2E is opt-in, destructive to controller-owned fixture branches/PRs,
and never part of ordinary CI. Use only the isolated LoopTest repository,
dedicated Linear fixture issue, selected-repository GitHub App, and the
[live-E2E runbook](runbooks/live-e2e.md).

The acceptance matrix requires:

| Boundary | Required evidence |
| --- | --- |
| Automatic admission | Bounded scan, unique priority selection, journaled reservation, exact Todo-to-In-Progress mutation, one run |
| Implementation | Owned worktree, resumable session, exact candidate, successful verifier batch |
| Internal review | Fresh independent read-only review bound to candidate head |
| Delivery | One owned branch/PR, required CI at exact head |
| Human feedback | Trusted root `CHANGES_REQUESTED`, one repair, new evidence, one fixed reply |
| Restart | Read-only unresolved-thread wait resumes without duplicate repair/reply/merge |
| Human authority | I-Fan resolves and approves exact repaired head in GitHub |
| Completion | Guarded merge, Linear completion observation, exact source sync, owned cleanup |
| Confidentiality | Sanitized retained evidence and clean credential scan |

Stop on unexpected target/actor/App, incomplete read, duplicated external write,
authority drift, protection mismatch, sensitive output, or unsafe source sync.
Never manually alter the fixture or database to manufacture a pass.

## Restart and Fault Injection

Restart tests should cut execution after persisted intent and before/after the
observable external effect, then reopen the same SQLite database in a new
controller process. Cover at least:

- worktree/artifact reservation and creation;
- Codex attempt start and session extraction;
- verifier start/interruption and full batch recording;
- fresh-review findings atomic handoff;
- Linear admission mutation intent/observation;
- push, PR create/adopt, review reply, and merge intent/observation;
- pending CI/approval/thread resolution polling;
- Linear completion observation;
- source sync and each cleanup resource.

Inject failures through ports, loopback fixtures, canceled contexts, managed
process fakes, or real disposable Git repositories. Never add production flags
that bypass safety solely to make fault injection easier.

## Database Migrations

SQLite migrations are ordered in `internal/adapters/sqlite/store.go`; the current
schema version is 22. Opening a database applies missing forward migrations in a
transaction. A database newer than the binary fails closed.

When adding a migration:

1. Add exactly one next version and update the schema constant.
2. Preserve prior evidence meaning; legacy rows must remain explicitly legacy
   or non-authoritative when the new invariant cannot be reconstructed.
3. Add fresh-database and upgrade-path tests, including restart/query behavior.
4. Update [Architecture](architecture.md) only when the persistence
   responsibility or invariant changes. Do not add migration-by-migration
   history to human docs.
5. Do not provide manual SQL as an operator recovery procedure.

## Adding a New State

1. Prove that an existing state plus typed evidence cannot represent the
   required durable stop/action.
2. Add the domain constant and minimal legal transitions.
3. Add topology tests for permitted and forbidden edges.
4. Define the state's authority, next action, waiting/terminal semantics, and
   restart behavior in application code.
5. Add atomic persistence/evidence tests and CLI/driver coverage.
6. Update the state machine and relevant module in
   [Architecture](architecture.md), plus operator behavior in
   [Operations](operations.md) when human-facing.

Do not add generic states that hide multiple external intents.

## Adding a New Side Effect

1. Define a narrow typed application port and request/result evidence.
2. Identify exact requester, repository, state, identity, and SHA gates.
3. Persist immutable intent and idempotency before invocation.
4. Implement bounded execution without shell interpolation or ambient
   credentials.
5. Define observation/adoption after ambiguous response and conflict behavior.
6. Add interruption fixtures before, during, and after the effect.
7. Add sanitized inspection evidence and credential-leak tests.
8. Update the authoritative architecture and operations sections; do not create
   a feature-slice document.

## Adding an External Adapter

Adapters implement existing or newly justified narrow ports. Validate endpoint
topology, identities, pagination/body/time bounds, credential source, and
sanitized errors at construction. External content must remain data. Keep
authentication material in memory, record only non-secret metadata/digests, and
prefer deterministic loopback fixtures over live tests.

If a future Hermes or HTTP adapter is added, it must call the same application
commands/queries and cannot own state, infer approval, or expose low-level state
buttons as a normal workflow.

## Documentation Checks

For documentation changes:

```sh
# Find all Markdown files and links.
rg --files -g '*.md' | sort
rg -n '\[[^]]+\]\([^)]+\.md(?:#[^)]+)?\)' -g '*.md'

# For each Markdown path deleted by the current diff, search its basename.
git diff --diff-filter=D --name-only -- '*.md'

git diff --check
./scripts/verify-controller.sh
```

Also verify relative links and anchors, compare every documented CLI name/flag
with `cmd/ifan-loop`, search retired terminology and obsolete commands, and run
the sensitive-output scan. Documentation must not contain credentials, real
personal IDs, authorization headers, private evidence, or absolute personal
paths.

## Pull Request Checklist

- Scope matches one current contract; unrelated findings have their own tracker
  item.
- Domain/application boundaries remain independent of concrete adapters.
- New external inputs are validated and never executed as shell text.
- State/evidence/SHA/idempotency and restart behavior are covered by tests.
- Structured contracts and command construction have focused tests.
- `gofmt`, tests, race tests, vet, diff check, fixture gate, and sensitive scan
  pass.
- Canonical documentation is updated without duplicated release-note history.
- The PR description includes summary, rationale, validation, out-of-scope
  notes, and the Linear magic word when Linear-managed.
