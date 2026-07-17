# Operations

## 1. Prerequisites

Production operation currently targets one local macOS user. Prepare:

- Go version declared by [`go.mod`](../go.mod);
- Git and a clean local source checkout for every configured repository;
- a compatible authenticated Codex CLI available by a fixed executable name or
  canonical absolute path;
- a Linear token with the configured IFAN read/state-transition access;
- a selected-repository GitHub App with only the required capabilities;
- the immutable GitHub `User` identity fields for each trusted operator;
- private controller state, worktree, artifact, credential, and log locations.

Use only an isolated fixture repository for live E2E work. Do not point a test
configuration at this controller repository or an STDS production repository.

## 2. Installation and Build

For development:

```sh
mkdir -p ./bin
go build -o ./bin/ifan-loop ./cmd/ifan-loop
./bin/ifan-loop version
```

For LaunchAgent use, install a current-user-owned, non-symlink executable
outside any repository checkout. It must have an execute bit and no group/world
write bit. Example:

```sh
mkdir -p "$HOME/.local/bin"
go build -o "$HOME/.local/bin/ifan-loop" ./cmd/ifan-loop
chmod 755 "$HOME/.local/bin/ifan-loop"
```

Stop the worker before replacing the installed binary. Re-run configuration and
LaunchAgent checks after every upgrade.

## 3. Filesystem Layout

The default macOS controller root is:

```text
~/Library/Application Support/agent-loop-controller/
  controller.json       secret-free configuration, mode 0600
  controller.db         authoritative workflow state and evidence
  secrets/              private directory, mode 0700
    linear-token        regular single-link file, mode 0600
  logs/                 private LaunchAgent logs, mode 0700
    worker.stdout.log   mode 0600
    worker.stderr.log   mode 0600
```

The GitHub App PEM is a separate protected regular file at the absolute path
named by configuration. Repository profiles separately name non-overlapping
source checkouts, run/artifact roots, and worktree roots. Do not place secrets
under any run root or repository.

`controller.db` and artifact directories are evidence, not editable operator
configuration. Never repair a run by editing SQLite or artifact JSON.

## 4. Configuration

Configuration version 3 is current. Versions 1 and 2 remain readable for
compatibility, but new installations should use `config init` and version 3.
The starter is intentionally incomplete and automatic admission is disabled.

The strict JSON document contains:

| Section | Responsibility |
| --- | --- |
| `controller` | Database path, Codex executable, and local action timeout |
| `linear` | Allowed GraphQL endpoint, credential source reference, team key, and bounded request settings |
| `github_app_profiles` | App/installation/repository identities, PEM reference, request bounds, and narrow write switches |
| `repositories` | Canonical owner/name, origins and local roots, base branch, verifier IDs, profile reference, and trusted actors |
| `automation.linear_todo_admission` | Disabled/enabled authority, exact workflow states, independent admission/delivery poll bounds, lease bounds, fixed requester, durable local event adapter mode, and credential reference |

Repository profiles are selectable one at a time per run. They may coexist in
one configuration, but paths must not overlap and a run freezes the selected
profile digest and private authority binding. Changing configuration never
retargets an active run.

Each repository may set `ci_slow_threshold` between `1m` and `24h`; omission
uses `20m`. This is an observability threshold, not a timeout. Required checks
that are absent, queued, or running continue to poll after one idempotent
`ci_wait_slow` event. The first persisted GitHub observation anchors the wait;
the controller wall clock evaluates the threshold even while GitHub reads are
temporarily unavailable. Existing version-1 profile digests created before this
optional field remain valid when the field is omitted.

Automatic admission uses a deterministic total order. Linear priorities 1, 2,
3, and 4 rank first in that order, and unprioritized 0 ranks last. Equal
priorities use the numeric sequence from the validated `IFAN-<sequence>`
identifier, ascending, with immutable issue UUID as a defensive final
comparator. The worker scans and revalidates the complete bounded set before
selection; response order, timestamps, title, assignee, and issue prose do not
affect the result. One nonterminal run blocks scanning and is never preempted.
Duplicate or contradictory identities and incomplete bounded scans still fail
closed for operator attention.

`automation.linear_todo_admission.poll_interval` controls only idle Linear Todo
admission scans and remains bounded from `1m` through `1h` (`5m` in the starter
configuration). `delivery_poll_interval` independently controls active-run
GitHub and Linear delivery observations and the driver's immediate-action
guard. It is bounded from `30s` through `5m`, with a `30s` default. This fixed
30-second strategy is the smallest deterministic MVP policy: one active run
makes at most roughly two delivery attempts or observations per minute while
avoiding the five-minute ready-state latency caused by inheriting admission
cadence. This bound includes retryable unavailable side-effect attempts; each
attempt still performs its normal fresh authority validation and idempotency
checks. Pending CI remains pending and is reread at this cadence; durable wait
evidence and the repository's 20-minute default slow-CI threshold are unchanged.

Enabled version-3 configurations created before this field existed remain
readable when `delivery_poll_interval` is omitted; the effective compatibility
default is `30s`, and `config inspect` projects that value. Operators should add
`"delivery_poll_interval": "30s"` explicitly on the next configuration update
so intent remains discoverable. An explicitly empty, `null`, malformed, below-
minimum, or above-maximum value is not an omission and fails validation. New
`config init` output always includes the explicit field.

## 5. Credentials and Permissions

The default Linear credential reference is `secret://file/linear-token`. The
file is re-read per request and must be a current-user-owned, non-symlink,
regular single-link file at mode `0600`, containing one non-empty token line and
at most one trailing newline. The legacy
`secret://env/IFAN_LOOP_LINEAR_TOKEN` source is explicit and never a fallback.

The GitHub App key file must be absolute, canonical, non-symlink, regular, and
not group/world accessible. Installation tokens are minted in memory and never
stored. Enable the three configuration write switches only when the App itself
has the matching selected-repository permission:

- `pull_requests_write`: create/adopt the one owned PR;
- `review_comments_write`: post one marker-bound reply to admitted feedback;
- `squash_merge_write`: conditional squash merge; requires Contents write.

Follow the high-risk [GitHub App setup runbook](runbooks/github-app.md). Do not
use personal `gh` credentials for controller delivery.

Every production command authenticates a complete immutable requester:

```text
--requester <login>
--requester-database-id <numeric-id>
--requester-node-id <node-id>
--requester-type User
```

These values must match the trusted actor in the frozen repository profile.

## 6. Normal Operator Workflow

### Configure and validate

```sh
ifan-loop config init
# Complete controller.json and provision external credentials.
ifan-loop config validate
ifan-loop config inspect
ifan-loop config doctor
```

Before enabling a live target, verify the selected repository identity, clean
base checkout, App installation/permissions, branch protection, required
checks, stale-approval dismissal, conversation resolution, and lack of bypass.

### Start automatic operation

Enable `automation.linear_todo_admission` only after validation. Then either run
the worker in the foreground:

```sh
ifan-loop controller worker
```

or install and supervise it with the LaunchAgent commands in section 8. The
worker receives no issue identifier. It resumes a nonterminal run before
scanning for a new Todo.

For one explicitly selected coding-ready issue, `controller run IFAN-123`
admits and drives that issue in the foreground. This is useful for deliberate
manual admission, recovery, and local controlled operation; it is not the
automatic Todo E2E trigger.

### Observe

The start/worker output exposes a run ID. Use requester-authorized queries:

```sh
ifan-loop controller status '<run-id>' <requester flags>
ifan-loop controller inspect '<run-id>' <requester flags>
```

The current implementation returns the same detailed safe projection for both
commands; use `status` for routine naming and `inspect` when investigating
evidence or recovery.

### Handle expected waits

- `awaiting_human_decision`: select one offered choice with a decision JSON,
  submit it through `controller continue`, then use `controller drive`.
- `pr_open`, `reconciling_reviews`: CI or GitHub evidence is being read.
- `awaiting_human_approval`: review/resolve/approve in GitHub; do not look for a
  controller approval command.
- `awaiting_github_mergeability`: GitHub protection or a human conversation is
  still blocking merge.
- `awaiting_linear_completion`: GitHub merged; Linear completion has not yet
  been observed.

### Recover after process or host restart

The database, not the process, owns progress. Start the worker again to resume
the one automatic run, or explicitly run:

```sh
ifan-loop controller drive '<run-id>' <requester flags>
```

Use `drive` when the prior foreground process ended because of a host restart,
signal, or the diagnostic `controller run`/`controller drive --max-runtime`,
and the persisted state remains a normal resumable state. Do not decompose the
workflow into recovery-only commands merely because the process stopped.

## 7. Command Reference

All examples omit `--config` when using the default path. Durations use Go
duration syntax such as `30s`, `15m`, and `24h`.

### Normal operator commands

### `version`

**Purpose**

Print the controller binary version.

**When to use**

Before an upgrade, compatibility check, or incident record.

**Syntax**

```sh
ifan-loop version
```

**Required arguments and flags**

None.

**Example**

```sh
ifan-loop version
```

**What it does**

Prints the build's version string and performs no I/O beyond stdout.

**Possible durable stop states**

None.

**Safety notes**

The version string does not prove configuration or CLI capability readiness.

**Related commands**

`config validate`, `controller launchagent doctor`.

### `config init`

**Purpose**

Create an absent secret-free version 3 starter configuration and private
credential directory.

**When to use**

Once for a new operator installation.

**Syntax**

```sh
ifan-loop config init [--config <controller.json>]
```

**Required arguments and flags**

None; `--config` overrides the default path.

**Example**

```sh
ifan-loop config init
```

**What it does**

Creates missing private directories, exclusively creates a mode-`0600` JSON
starter, and reports that setup remains required. It does not create tokens,
keys, profiles, repositories, or a runnable worker.

**Possible durable stop states**

None.

**Safety notes**

Refuses to overwrite an existing file or repair an unsafe existing secret
directory.

**Related commands**

`config path`, `config validate`, `config doctor`.

### `config path`

**Purpose**

Report the resolved configuration path.

**When to use**

Before editing or scripting around the default location.

**Syntax**

```sh
ifan-loop config path [--config <controller.json>]
```

**Required arguments and flags**

None.

**Example**

```sh
ifan-loop config path
```

**What it does**

Returns a JSON `path`; it does not open the configuration.

**Possible durable stop states**

None.

**Safety notes**

Treat an absolute path as private host metadata when sharing output.

**Related commands**

`config init`, `config validate`.

### `config validate`

**Purpose**

Strictly load and cross-check configuration offline.

**When to use**

After every configuration change and before starting a worker.

**Syntax**

```sh
ifan-loop config validate [--config <controller.json>]
```

**Required arguments and flags**

None.

**Example**

```sh
ifan-loop config validate --config /absolute/private/controller.json
```

**What it does**

Validates schema, endpoints, identities, paths, profile cross-references,
automation authority, and limits; returns the credential-safe readiness
projection. It performs no network request and does not read credential bytes.

**Possible durable stop states**

None; invalid configuration exits with an error.

**Safety notes**

Offline success is not credential or external-permission readiness.

**Related commands**

`config inspect`, `config doctor`, `controller launchagent doctor`.

### `config inspect`

**Purpose**

Inspect the same sanitized offline configuration projection.

**When to use**

To confirm configuration/profile digests and enabled automation bounds.

**Syntax**

```sh
ifan-loop config inspect [--config <controller.json>]
```

**Required arguments and flags**

None.

**Example**

```sh
ifan-loop config inspect
```

**What it does**

Reports configuration version/digest, profile identities/digests, non-secret
limits, credential source type, and enabled state without network/database I/O.

**Possible durable stop states**

None.

**Safety notes**

It deliberately omits credential references, workflow state IDs, private key
contents, and secret paths.

**Related commands**

`config validate`, `config doctor`.

### `config doctor`

**Purpose**

Check safe Linear credential-source topology at runtime.

**When to use**

After provisioning or rotating the Linear credential and before worker start.

**Syntax**

```sh
ifan-loop config doctor [--config <controller.json>]
```

**Required arguments and flags**

None.

**Example**

```sh
ifan-loop config doctor
```

**What it does**

Loads configuration and reports credential readiness or a generic warning. It
does not perform network I/O or print the source reference, path, token, or
underlying filesystem error.

**Possible durable stop states**

None.

**Safety notes**

A ready result does not validate token scope or GitHub App access.

**Related commands**

`config validate`, `controller launchagent doctor`.

### `controller worker`

**Purpose**

Run automatic Linear Todo admission and the production driver.

**When to use**

For normal automatic operation, directly or under LaunchAgent supervision.

**Syntax**

```sh
ifan-loop controller worker [--config <controller.json>] [--once]
```

**Required arguments and flags**

No positional argument. `--once` performs one resume or scan/dispatch cycle.
Normal worker operation has no process-lifetime expiry; operation-specific
network, process, verification, and control timeouts remain bounded.

**Example**

```sh
ifan-loop controller worker --once
```

**What it does**

Validates automation authority and credential topology, acquires the singleton
scheduler lease, resumes a nonterminal run or scans/adopts one eligible Todo,
then drives it. Attention parks admission but does not terminate the worker; a
later cycle keeps observing the same durable authority without admitting a
second run. The short scheduler lease is released after every cycle rather than
renewed while parked. It reports bounded worker and queue-decision evidence;
`status` is `running`, `driving`, `parked`, or `stopping`, and a stopping result
includes `previous_status`. The worker atomically replaces the private
`<controller-config>.worker-status.json` snapshot on each transition rather
than appending cadence logs. `controller launchagent status` projects its
`worker_status`, `worker_previous_status`, and observation timestamp while the
LaunchAgent process is observed running and its launchctl PID matches the
snapshot PID and OS process-start identity. A missing or stale snapshot falls
back to launchd's sanitized `running` observation rather than projecting the
previous worker instance, including after PID reuse.

**Possible durable stop states**

Disabled policy, `--once`, SIGINT, or SIGTERM. Human decision, manual
intervention, retry attention, incomplete scan, no candidate, and terminal run are
durable outcomes observed by the continuing worker, not process expiry.

**Safety notes**

Never pass an issue identifier. SIGINT/SIGTERM cancels the active driver and its
children, stops lease renewal, performs bounded lease cleanup, closes SQLite,
and emits a sanitized `stopped: canceled` result. It does not rewrite the run
as failed or abandoned. An unexpected failure exits nonzero so LaunchAgent can
restart and resume from persisted state without duplicate admission.

**Related commands**

`controller status`, `controller inspect`, `controller drive`, LaunchAgent
commands.

### `controller run`

**Purpose**

Admit one explicit Linear issue and drive its persisted workflow.

**When to use**

For deliberate manual admission or controlled recovery; not for automatic Todo
acceptance tests.

**Syntax**

```sh
ifan-loop controller run <IFAN-issue> [--config <file>] <requester flags> \
  [--poll-interval <duration>] [--max-immediate-actions <n>] \
  [--max-runtime <duration>]
```

**Required arguments and flags**

One issue identifier and complete requester identity. Poll defaults to `30s`,
immediate actions to `32`, and runtime to `24h` (maximum `168h`).

**Example**

```sh
ifan-loop controller run IFAN-123 \
  --requester operator --requester-database-id 123 \
  --requester-node-id '<node-id>' --requester-type User
```

**What it does**

Reads and admits the issue, prints the run ID to stderr, constructs all bounded
production adapters, and drives until a durable stop or process bound.

**Possible durable stop states**

Any human/manual/terminal state; pending GitHub and Linear states are polled
within the process.

**Safety notes**

Eligibility and profile authority still apply. Repeated calls do not authorize
a second conflicting run.

**Related commands**

`controller drive`, `controller status`, `controller worker`.

### `controller drive`

**Purpose**

Resume the production driver for an already admitted run.

**When to use**

After a foreground process or host restart, or after a valid human decision.

**Syntax**

```sh
ifan-loop controller drive <run-id> [--config <file>] <requester flags> \
  [--poll-interval <duration>] [--max-immediate-actions <n>] \
  [--max-runtime <duration>]
```

**Required arguments and flags**

Run ID and complete requester identity; policy bounds match `controller run`.

**Example**

```sh
ifan-loop controller drive '<run-id>' <requester flags>
```

**What it does**

Authorizes the requester, derives repository/state/idempotency authority from
SQLite, reconstructs adapters, and continues the legal next-action loop.

**Possible durable stop states**

Human decision, manual intervention, terminal state, cancellation, or runtime
limit.

**Safety notes**

Inspect a manual-intervention stop before retrying; `drive` is not an override.

**Related commands**

`controller status`, `controller inspect`, `controller continue`.

### `controller status`

**Purpose**

Read a requester-authorized safe run projection.

**When to use**

Routine observation and before any recovery action.

**Syntax**

```sh
ifan-loop controller status <run-id> [--config <file>] <requester flags>
```

**Required arguments and flags**

Run ID and complete requester identity.

**Example**

```sh
ifan-loop controller status '<run-id>' <requester flags>
```

**What it does**

Reads SQLite only and returns run, timeline, attempts, evidence, owned resources,
external observations, and safe recovery fields.

**Possible durable stop states**

Does not change state.

**Safety notes**

Protect retained output as operational evidence even though it is sanitized.

**Related commands**

`controller inspect`, `controller drive`.

### `controller inspect`

**Purpose**

Inspect detailed persisted evidence for diagnosis or recovery.

**When to use**

Before every low-level or typed recovery command.

**Syntax**

```sh
ifan-loop controller inspect <run-id> [--config <file>] <requester flags>
```

**Required arguments and flags**

Run ID and complete requester identity.

**Example**

```sh
ifan-loop controller inspect '<run-id>' <requester flags>
```

**What it does**

Currently returns the same detailed projection as `status`; its name signals
diagnostic intent.

**Possible durable stop states**

Does not change state.

**Safety notes**

Use the persisted repository, current state, and idempotency key exactly; never
copy values from a different run.

**Related commands**

All recovery-only commands.

### Human action commands

### `controller continue --decision`

**Purpose**

Submit one structured human choice and continue one legal local action.

**When to use**

Only at `awaiting_human_decision` with the exact offered options shown by
inspection.

**Syntax**

```sh
ifan-loop controller continue <run-id> [--config <file>] <requester flags> \
  --repository <owner/name> --expected-state awaiting_human_decision \
  --idempotency-key <persisted-key> --decision <decision.json>
```

**Required arguments and flags**

Run ID, complete requester, repository, expected state, idempotency key, and a
decision file when answering a human gate. The JSON shape is:

```json
{
  "choice_id": "one-persisted-offered-id",
  "instructions": "Bounded clarification for the selected option."
}
```

**Example**

```sh
ifan-loop controller continue '<run-id>' <requester flags> \
  --repository owner/repository \
  --expected-state awaiting_human_decision \
  --idempotency-key '<persisted-key>' --decision /private/decision.json
```

**What it does**

Revalidates Linear, binds the selected offered choice to its exact originating
outcome, persists the decision, and advances/resumes the local controller.
When the long-running worker is active, its next poll automatically returns the
same run to the production driver; do not issue a separate `controller drive`.

**Possible durable stop states**

Executing, verifying, fresh review, another human decision, manual intervention,
or terminal failure.

**Safety notes**

Do not alter the decision request or invent a choice ID. The command also has a
recovery-only no-decision use; normal progression should use `drive`.

**Related commands**

`controller inspect`, `controller drive`.

GitHub review feedback, conversation resolution, and approval are human actions
performed in GitHub, not CLI commands. After acting, leave the driver running or
resume it with `controller drive`.

### Recovery-only commands

> These commands are not a normal delivery recipe. Do not manually reproduce a
> complete run state by state. First use `status` and `inspect`; then use only
> the persisted requester identity, repository, expected state, and idempotency
> evidence. Every command still derives and enforces the legal transition.

The shared syntax for most commands is:

```sh
ifan-loop controller <command> <run-id> [--config <file>] <requester flags> \
  --repository <owner/name> --expected-state <persisted-state> \
  --idempotency-key <persisted-key>
```

`continue`, `push`, `open-pr`, `reconcile`, `merge`, `reconcile-linear`, and
`cleanup` each invoke one coordinator action. A mismatched state either reports
the derived action or fails; it cannot be used to jump ahead.

### `controller retry`

**Purpose**

Grant one additional, auditable execution eligibility to a run parked because
its typed automatic retry budget was exhausted.

**When to use**

Only when `controller inspect` shows the current automatic retry schedule in
`attention` with reason `retry_budget_exhausted` and failure class
`process_start` while the run is still between `received` and `approval_ready`,
before GitHub delivery authority, or `unavailable` while still in `received` or
`admitting`. Once push or pull-request delivery begins, use the state-specific
inspection and recovery path instead of this command.

**Syntax**

```sh
ifan-loop controller retry '<run-id>' [--config <file>] <requester flags>
```

**Required arguments and flags**

The run ID and all requester identity flags. Repository, state, transition
sequence, Linear revision, idempotency key, retry reason, and local ownership
come only from persisted controller authority; this command does not accept
caller-supplied replacements.

**Example**

```sh
ifan-loop controller retry '<run-id>' \
  --requester ifan0927 --requester-database-id 123 \
  --requester-node-id '<github-node-id>' --requester-type User
```

**What it does**

Revalidates the requester, unchanged Linear source, exact run authority,
current attention, resolved side effects, and controller-owned local resources.
It first records the typed operator intent, then atomically marks that exact
exhausted schedule as immediately eligible with reason `operator_retry`. A
running worker detects the persisted eligibility on its next poll and resumes
the normal production driver automatically.

**Possible durable stop states**

The controller state is unchanged by the command. The schedule becomes
`scheduled`; after the worker runs, normal driver states apply. If that attempt
fails again, its incremented attempt receives a new stable attention event.

**Safety notes**

The command does not rewind state, delete evidence, reset attempts or limits,
answer human decisions, approve or merge, abandon, or override authority and
integrity failures. It cannot unlock push, pull-request, review, or merge
states. Exact replay returns the same journaled result.

**Related commands**

`controller inspect`, `controller worker`, `controller drive`.

### `controller recover-ci-wait`

This one-purpose compatibility recovery applies only to a pre-fix run parked at
`pr_open` or `reconciling_reviews` by the historical check-topology-drift read.

```sh
ifan-loop controller recover-ci-wait '<run-id>' [--config <file>] \
  --requester '<login>' --requester-database-id '<id>' \
  --requester-node-id '<node-id>' --requester-type User
```

It requires the exact 13-observation incident fingerprint (including token
mint), successful read transport, unchanged Linear/requester/profile/App/PR/
head/base/local ownership, and no unresolved side-effect intent. Fresh GitHub
evidence must contain complete required-check authority. It performs fresh
read-only Linear and GitHub validation, records typed operator-action provenance,
supersedes only the matching terminal schedule, and lets the running worker
resume on its next poll. It never pushes, opens or adopts a PR, replies,
approves, resolves, merges, or invokes the driver.

### `controller continue`

**Purpose**

Execute or reconcile one local-controller action without running the long-lived
driver.

**When to use**

Incident recovery or deliberate fault injection; use the decision form above
for a human-decision stop.

**Syntax**

Use the shared recovery syntax, optionally with `--decision <file>`.

**Required arguments and flags**

All shared authority flags.

**Example**

```sh
ifan-loop controller continue '<run-id>' <requester flags> \
  --repository owner/repo --expected-state executing \
  --idempotency-key '<persisted-key>'
```

**What it does**

Revalidates Linear and performs the one local action derived from current state.

**Possible durable stop states**

Any local execution/review, human, manual, or terminal state.

**Safety notes**

Prefer `drive`; do not repeatedly invoke on an unavailable process without
checking retry and attempt evidence.

**Related commands**

`controller drive`, `controller inspect`.

### `controller push`

**Purpose**

Reconcile or publish the verified current candidate to its owned branch.

**When to use**

Only for a halted `approval_ready`/`pushing_branch` incident.

**Syntax**

Use the shared recovery syntax.

**Required arguments and flags**

All shared authority flags.

**Example**

```sh
ifan-loop controller push '<run-id>' <requester flags> \
  --repository owner/repo --expected-state pushing_branch \
  --idempotency-key '<persisted-key>'
```

**What it does**

Revalidates owned worktree/origin/branch/head and exact-head verification/review,
observes the remote, and performs only the permitted explicit ref update.

**Possible durable stop states**

`branch_pushed`, `manual_intervention`, or failure.

**Safety notes**

Never substitute a branch or SHA. A divergent remote is not automatically
overwritten.

**Related commands**

`recover-owned-push`, `controller drive`.

### `controller recover-owned-push`

**Purpose**

Return one evidence-proven halted owned-PR repair push to the guarded push gate.

**When to use**

Only for `manual_intervention` caused by a repair fast-forward on an existing
controller-owned open PR.

**Syntax**

Use the shared recovery syntax with expected state `manual_intervention`.

**Required arguments and flags**

All shared authority flags.

**Example**

```sh
ifan-loop controller recover-owned-push '<run-id>' <requester flags> \
  --repository owner/repo --expected-state manual_intervention \
  --idempotency-key '<persisted-key>'
```

**What it does**

Proves stable Linear source and persisted PR ownership, then transitions to
`approval_ready`. It performs no Git or GitHub write; the resumed driver repeats
all push gates.

**Possible durable stop states**

`approval_ready` or unchanged `manual_intervention` on rejection.

**Safety notes**

Not a general remote-divergence override.

**Related commands**

`controller push`, `controller drive`.

### `controller open-pr`

**Purpose**

Create or adopt the one controller-owned pull request.

**When to use**

Only for a halted `branch_pushed`/`opening_pr` incident with the configured App
write capability.

**Syntax**

Use the shared recovery syntax.

**Required arguments and flags**

All shared authority flags; GitHub App `pull_requests_write=true`.

**Example**

```sh
ifan-loop controller open-pr '<run-id>' <requester flags> \
  --repository owner/repo --expected-state opening_pr \
  --idempotency-key '<persisted-key>'
```

**What it does**

Uses persisted ownership marker/body digest and exact head/base to adopt an
exact PR or create one after intent is durable.

**Possible durable stop states**

`pr_open`, `manual_intervention`, or failure.

**Safety notes**

Matching branch/title alone is insufficient and must remain insufficient.

**Related commands**

`controller reconcile`, `controller drive`.

### `controller reconcile`

**Purpose**

Perform one fresh GitHub read/reconciliation action.

**When to use**

To diagnose or advance a stopped PR/check/review/mergeability poll.

**Syntax**

Use the shared recovery syntax.

**Required arguments and flags**

All shared authority flags and readable GitHub App configuration.

**Example**

```sh
ifan-loop controller reconcile '<run-id>' <requester flags> \
  --repository owner/repo --expected-state awaiting_human_approval \
  --idempotency-key '<persisted-key>'
```

**What it does**

Revalidates Linear as applicable, reads the exact owned PR/check/review/thread
topology, persists observations, and selects wait, repair, reply, merge, or
manual intervention.

**Possible durable stop states**

Review reconciliation, reply, repair, approval wait, mergeability wait, merge,
manual intervention, or terminal failure.

**Safety notes**

Pending CI or unresolved conversation is normal; do not loop this command
rapidly in place of the driver's bounded poll.

**Related commands**

`controller drive`, `controller merge`.

### `controller merge`

**Purpose**

Reconcile or perform the guarded conditional squash merge.

**When to use**

Only for a halted `merging` incident after exact-head gates are visibly present.

**Syntax**

Use the shared recovery syntax.

**Required arguments and flags**

All shared authority flags; GitHub App `squash_merge_write=true`.

**Example**

```sh
ifan-loop controller merge '<run-id>' <requester flags> \
  --repository owner/repo --expected-state merging \
  --idempotency-key '<persisted-key>'
```

**What it does**

Re-reads PR/head/base/check/review/approval authority, persists merge intent,
sends an exact-head squash request, and observes the result.

**Possible durable stop states**

`awaiting_github_mergeability`, `awaiting_linear_completion`,
`manual_intervention`, or failure.

**Safety notes**

It never bypasses protection, changes merge method, or approves the PR.

**Related commands**

`controller reconcile`, `accept-external-merge`.

### `controller accept-external-merge`

**Purpose**

Explicitly accept an externally merged owned PR after proving it delivered the
verified candidate tree.

**When to use**

Only when such a merge has already placed the run in matching
`manual_intervention`.

**Syntax**

Use the shared recovery syntax with expected state `manual_intervention`.

**Required arguments and flags**

All shared authority flags.

**Example**

```sh
ifan-loop controller accept-external-merge '<run-id>' <requester flags> \
  --repository owner/repo --expected-state manual_intervention \
  --idempotency-key '<persisted-key>'
```

**What it does**

Revalidates candidate verification/review/checks/approval, remote-base
containment, and candidate/merge tree equality; records merge method `external`
and moves to Linear completion observation.

**Possible durable stop states**

`awaiting_linear_completion` or unchanged manual intervention.

**Safety notes**

It does not automatically accept all manual merges or weaken the normal squash
path.

**Related commands**

`controller reconcile-linear`, `controller drive`.

### `controller reconcile-linear`

**Purpose**

Perform one bounded Linear completion observation after merge.

**When to use**

Only for a halted `awaiting_linear_completion` incident.

**Syntax**

Use the shared recovery syntax.

**Required arguments and flags**

All shared authority flags and Linear credential readiness.

**Example**

```sh
ifan-loop controller reconcile-linear '<run-id>' <requester flags> \
  --repository owner/repo --expected-state awaiting_linear_completion \
  --idempotency-key '<persisted-key>'
```

**What it does**

Re-reads the issue, binds its observed completion state to the recorded merge,
and advances to cleanup only when authoritative.

**Possible durable stop states**

`awaiting_linear_completion`, `cleaning`, `manual_intervention`, or failure.

**Safety notes**

This is read-only; fix missing external automation in Linear rather than editing
controller state.

**Related commands**

`controller cleanup`, `controller drive`.

### `controller cleanup`

**Purpose**

Retry source synchronization and unfinished owned-resource cleanup.

**When to use**

Only for a halted `cleaning` incident after inspecting each resource result.

**Syntax**

Use the shared recovery syntax.

**Required arguments and flags**

All shared authority flags.

**Example**

```sh
ifan-loop controller cleanup '<run-id>' <requester flags> \
  --repository owner/repo --expected-state cleaning \
  --idempotency-key '<persisted-key>'
```

**What it does**

Reconciles safe source sync and retries only incomplete controller-owned
worktree/local/remote branch cleanup records.

**Possible durable stop states**

`completed`, `cleaning`, or `manual_intervention`.

**Safety notes**

Do not clean a dirty source checkout manually to make evidence pass. Resolve the
operator-owned checkout deliberately, then re-inspect.

**Related commands**

`controller inspect`, `controller drive`.

### `controller abandon`

**Purpose**

Gracefully terminalize one eligible parked run while preserving evidence and
releasing the singleton worker slot even when cleanup leaves residue.

**When to use**

Only when the current operator-attention event advertises `abandon`. An observed
merge, merge intent, authority drift, missing fresh PR evidence, or an active
run lease remains fail-closed.

**Syntax**

The command loads repository, current-state, sequence, and idempotency authority
from SQLite after authenticating the requester.

**Required arguments and flags**

Run ID, config path when non-default, and the complete requester identity flags.

**Example**

```sh
ifan-loop controller abandon '<run-id>' <requester flags>
```

**What it does**

Revalidates the immutable Linear task and repository authority plus the exact
parked attention. A recognized Linear PR-automation move into another
`started` workflow state is accepted only when the issue identity, task
content, repository, working branch, and requester authority remain unchanged.
The command then records the typed operator action intent and classifies durable
ownership evidence. A `prepared` attempt is durable evidence that no process
launch was authorized; abandon can terminalize it without inventing missing
process evidence. Under the acquired run lease the controller authenticates
every durably `started` managed process group with its SQLite-held per-attempt key,
bound lock inode, exact kernel process-start identity, and authenticated launch
roster. Every roster entry must remain complete; finding only an older preflight
identity cannot authorize cleanup. The controller keeps
the lifecycle lock on a private open-file description that is never inherited
by the child, so the child cannot release or replace its authority. A managed
launch supervisor prevents the requested target from executing until this
identity is durable. The supervisor remains the group leader and drains the
trusted Codex target and other members of that process group before it reports
completion. It does not claim adversarial containment if a trusted executable
deliberately creates another process group or session; that isolation is outside
the local macOS MVP. After a controller crash, a restart claims the same
authenticated lock inode before signaling the surviving group. An orphan lock, missing
identity, or leaderless-but-live process group is ambiguous and fails closed.
The controller revalidates the kernel start token, the leader's current kernel
process-group membership, the live process group, and controller-held lock
immediately before each interrupt or kill signal and on every bounded exit-proof
poll; stale pre-signal observations are not authority.
If identity authentication or exit proof is unavailable, resource cleanup is
skipped and retained as residue. It then
applies persisted best-effort cleanup to unchanged
owned worktrees and local branches. A remote branch is cleaned only when a
freshly observed open controller-owned PR proves unmerged delivery authority; a
remote branch without that PR authority is retained. A freshly observed closed, unmerged,
controller-owned PR is adopted as cleaned. For an open PR, the controller does
not close it directly; it re-reads GitHub after local cleanup, then deletes the
exact owned remote branch only if that final read still proves the PR open,
unmerged, and owned. If that final read fails or reports merged or mismatched
authority, remote resources are retained as residue while terminalization still
completes. A PR that remains open is retained as residue. Artifacts are always
retained. The run then becomes
terminal `failed`
with `operator_abandoned` even if an unsafe or failed resource produces
cleanup-residue attention. Retained retry history no longer blocks the worker
from scanning another issue. Replay adopts the terminal action and never
repeats cleanup already recorded successful. Once action intent is durable,
request cancellation becomes cleanup residue
while a separate controller-bounded context completes terminalization. Cleanup
uses a narrower deadline; exhausting it retains residue while the still-valid
terminalization budget records `failed` with `operator_abandoned`. Replay
repairs a missing action result from the persisted terminal transition.
If a prior invocation persisted
remote-deletion intent and Git accepted the deletion but the local result write
was interrupted, replay first proves every managed child exited, then
authenticates that exact ownership and freshly proves the remote
ref absent, records the deletion, and accepts only a current unmerged PR status
read whose missing head is explained by that deletion. Other GitHub drift still
fails closed. A terminal replay performs cleanup probes only while the current
attention is the exact cleanup-residue event; without that attention it returns
the already-terminal result as an idempotent no-op.

**Possible durable stop states**

`failed` or unchanged prior state on rejected preconditions.

**Safety notes**

It never changes Linear or closes a PR, never deletes unknown or drifted
resources, and never interprets a database attempt status as OS-process stop
evidence. A persisted PR
requires a fresh GitHub read; merged evidence is rejected in favor of explicit
external-merge recovery. Inspect `cleanup_progress`,
`operator_attention_events`, and `operator_actions` for retained operator work.

**Related commands**

`controller inspect`, `controller worker`.

### Compatibility and diagnostic commands

### `linear start` / `controller start`

**Purpose**

Use the compatibility manual-admission route for one explicit Linear issue.

**When to use**

For bounded admission/local-controller diagnosis when the long-lived production
driver is intentionally not desired. Prefer `controller run` otherwise.

**Syntax**

```sh
ifan-loop linear start <IFAN-issue> [--config <file>] <requester flags>
ifan-loop controller start <IFAN-issue> [--config <file>] <requester flags>
```

**Required arguments and flags**

One IFAN identifier and complete requester identity. The two forms route to the
same implementation.

**Example**

```sh
ifan-loop linear start IFAN-123 <requester flags>
```

**What it does**

Reads/admit the authoritative Linear issue and invokes the bounded local
controller. It does not construct the long-lived GitHub delivery driver.

**Possible durable stop states**

Any local implementation, verification, fresh-review, human-decision, manual,
or terminal state.

**Safety notes**

Do not use it to manually replace automatic worker admission in the live E2E.

**Related commands**

`controller run`, `controller drive`, `controller inspect`.

### `github-read`

**Purpose**

Exercise the direct read-only GitHub App evidence adapter for an already
persisted run and PR.

**When to use**

Only for focused adapter diagnosis or an explicitly authorized isolated smoke.

**Syntax**

```sh
ifan-loop github-read [--config <file>] --run-id <run-id> \
  <requester flags> --repository <owner/name> \
  --expected-state <state> --idempotency-key <persisted-key> \
  --pr <number> --expected-head <sha>
```

**Required arguments and flags**

Complete run/requester/repository/state/idempotency authority, positive PR
number, and exact expected head.

**Example**

```sh
ifan-loop github-read --run-id '<run-id>' <requester flags> \
  --repository owner/repo --expected-state awaiting_human_approval \
  --idempotency-key '<persisted-key>' --pr 1 --expected-head '<sha>'
```

**What it does**

Authorizes against SQLite and the frozen profile, performs bounded REST/GraphQL
reads through the configured App, persists sanitized request/evidence records,
and returns the normalized result.

**Possible durable stop states**

It does not intentionally advance normal workflow state; failures may persist
read observations for diagnosis.

**Safety notes**

Never use an arbitrary PR/head or a broad-production App as a connectivity
probe. It does not consult personal `gh` credentials.

**Related commands**

`controller reconcile`, `controller inspect`.

### Development-only commands

`plan`, `spike`, and `local start|continue|status|inspect|fixture-deliver` are
fixture/development interfaces. Their inputs, safety boundary, and current
scripts are documented in [Development](development.md). They are not supported
production workflow steps.

## 8. Automatic Worker and LaunchAgent

The embedded LaunchAgent runs exactly:

```text
<absolute-binary> controller worker --config <absolute-config>
```

It uses label `com.ifan.agent-loop-controller.worker`, `RunAtLoad`, restart only
after unsuccessful exit, 30-second throttle, umask `0077`, and private stdout/
stderr files. No token, requester, issue, branch, shell, checkout, or environment
entry is rendered into the plist.

The supervised worker is indefinite: the plist never supplies `--once` or a
process lifetime. Binary and configuration changes are restart-to-reload; boot
out the service, perform the validated replacement, then bootstrap it again.
Bootout is process control only and does not mark a run failed or abandoned.
`doctor` and LaunchAgent control results advertise
`process_lifetime: indefinite` and `log_policy: startup_truncate_8_mib`.

All LaunchAgent commands share:

```text
--binary <absolute-installed-binary>   default /usr/local/bin/ifan-loop
--config <absolute-controller.json>    default controller configuration
--plist <absolute-plist>               default user LaunchAgents path
--domain gui/<uid>                     default current GUI user
--timeout <duration>                   default 15s, maximum 2m
```

### `controller launchagent doctor`

**Purpose**

Read-only preflight of binary, configuration, database parent, credential, and
log safety.

**When to use**

Before render/install and after upgrades.

**Syntax**

```sh
ifan-loop controller launchagent doctor [common flags]
```

**Required arguments and flags**

No positional arguments; supply the actual installed binary and configuration.

**Example**

```sh
ifan-loop controller launchagent doctor --binary "$HOME/.local/bin/ifan-loop"
```

**What it does**

Returns finite reason codes without repairing files or exposing paths/secrets.

**Possible durable stop states**

No run state change.

**Safety notes**

Create private log directory/leaves before expecting readiness.

**Related commands**

`config doctor`, `controller launchagent validate`.

### `controller launchagent validate`

**Purpose**

Run doctor checks plus absent-target install preflight.

**When to use**

Immediately before first install.

**Syntax**

```sh
ifan-loop controller launchagent validate [common flags]
```

**Required arguments and flags**

No positional arguments.

**Example**

```sh
ifan-loop controller launchagent validate --binary "$HOME/.local/bin/ifan-loop"
```

**What it does**

Reports `plist_exists` rather than overwriting an existing target.

**Possible durable stop states**

No run state change.

**Safety notes**

An existing exact installed plist is handled by `install`; validate remains a
pre-install check.

**Related commands**

`build`, `install`.

### `controller launchagent build` / `render`

**Purpose**

Render the exact versioned plist to stdout (`build` and `render` are aliases).

**When to use**

For review and `plutil -lint` before install.

**Syntax**

```sh
ifan-loop controller launchagent build [common flags]
```

**Required arguments and flags**

No positional arguments.

**Example**

```sh
ifan-loop controller launchagent build --binary "$HOME/.local/bin/ifan-loop" > /tmp/worker.plist
plutil -lint /tmp/worker.plist
```

**What it does**

Renders only; it does not build the Go binary or write/install a plist.

**Possible durable stop states**

No run state change.

**Safety notes**

Keep the rendered document secret-free and do not add environment credentials.

**Related commands**

`install`, `plist-validate`.

### `controller launchagent install`

**Purpose**

Exclusively install the exact rendered plist.

**When to use**

After doctor/validate and independent `plutil` review.

**Syntax**

```sh
ifan-loop controller launchagent install [common flags]
```

**Required arguments and flags**

No positional arguments.

**Example**

```sh
ifan-loop controller launchagent install --binary "$HOME/.local/bin/ifan-loop"
```

**What it does**

Uses safe directory/file identity checks and exclusive creation. An identical
existing document is idempotent; a different one is refused.

**Possible durable stop states**

No run state change.

**Safety notes**

It never loads the service or overwrites an unknown plist.

**Related commands**

`plist-validate`, `bootstrap`.

### `controller launchagent plist-validate`

**Purpose**

Statically validate the installed plist contract.

**When to use**

After installation and before bootstrap.

**Syntax**

```sh
ifan-loop controller launchagent plist-validate [common flags]
```

**Required arguments and flags**

No positional arguments.

**Example**

```sh
ifan-loop controller launchagent plist-validate --binary "$HOME/.local/bin/ifan-loop"
```

**What it does**

Checks exact label, worker argv, paths, and `RunAtLoad` without invoking
`launchctl`.

**Possible durable stop states**

No run state change.

**Safety notes**

Static validity does not prove the service can start.

**Related commands**

`bootstrap`, `status`.

### `controller launchagent bootstrap`

**Purpose**

Load the exact service into the GUI domain.

**When to use**

After installed-plist validation.

**Syntax**

```sh
ifan-loop controller launchagent bootstrap [common flags]
```

**Required arguments and flags**

No positional arguments.

**Example**

```sh
ifan-loop controller launchagent bootstrap --binary "$HOME/.local/bin/ifan-loop"
```

**What it does**

Performs one bounded launchctl bootstrap and observation; an already loaded
service is reconciled.

**Possible durable stop states**

The worker may itself reach any normal worker stop.

**Safety notes**

A timeout is operator attention, not assumed success; run `status` next.

**Related commands**

`status`, `kickstart`, `bootout`.

### `controller launchagent status`

**Purpose**

Observe the exact LaunchAgent service.

**When to use**

After every control timeout, start, stop, or unexpected worker exit.

**Syntax**

```sh
ifan-loop controller launchagent status [common flags]
```

**Required arguments and flags**

No positional arguments.

**Example**

```sh
ifan-loop controller launchagent status --binary "$HOME/.local/bin/ifan-loop"
```

**What it does**

Returns a finite observed state/outcome/next action without exposing raw
`launchctl` output.

**Possible durable stop states**

No controller run state change.

**Safety notes**

Also inspect private worker logs and controller run state when the service has
exited.

**Related commands**

`kickstart`, `bootout`, `controller inspect`.

### `controller launchagent kickstart`

**Purpose**

Start one loaded service that is stably stopped/exited.

**When to use**

After status shows a safe kickstart action.

**Syntax**

```sh
ifan-loop controller launchagent kickstart [common flags]
```

**Required arguments and flags**

No positional arguments.

**Example**

```sh
ifan-loop controller launchagent kickstart --binary "$HOME/.local/bin/ifan-loop"
```

**What it does**

Observes first, avoids duplicating an already running/initially scheduled start,
then performs one bounded kickstart when permitted.

**Possible durable stop states**

The worker may reach any normal worker stop.

**Safety notes**

Do not repeatedly kickstart an attention/failure loop without reading logs and
run evidence.

**Related commands**

`status`, `controller inspect`.

### `controller launchagent bootout`

**Purpose**

Stop and unload the exact service without deleting its plist.

**When to use**

Before upgrades, log rotation, configuration maintenance, or uninstall.

**Syntax**

```sh
ifan-loop controller launchagent bootout [common flags]
```

**Required arguments and flags**

No positional arguments.

**Example**

```sh
ifan-loop controller launchagent bootout --binary "$HOME/.local/bin/ifan-loop"
```

**What it does**

Stops the exact label once; an absent service is idempotent.

**Possible durable stop states**

The active run remains persisted and resumable.

**Safety notes**

Confirm with `status` before replacing binaries, logs, or plist files.

**Related commands**

`status`, `bootstrap`, `controller drive`.

## 9. Human Decision Workflow

1. Inspect `awaiting_human_decision` and locate the exact decision request and
   offered option IDs.
2. Discuss the choice outside the controller if necessary; do not edit task or
   database evidence to encode the answer.
3. Create a private bounded JSON decision with one offered `choice_id`.
4. Submit it through the fully authorized `controller continue` command.
5. Re-inspect; if the process stopped after the one local action, use
   `controller drive`.

A changed Linear contract is a separate source-drift concern. Do not disguise a
material task change as free-form decision instructions.

## 10. GitHub Review and Approval Workflow

1. The controller opens one PR only after verification and fresh review pass.
2. Required CI and review topology are polled by the driver.
3. I-Fan may submit an exact-head inline root `CHANGES_REQUESTED` review. The
   controller authenticates it, repairs, re-verifies/re-reviews, pushes a new
   head, and posts one fixed reply.
4. I-Fan reviews the repair and resolves the conversation when satisfied. The
   controller never resolves it.
5. I-Fan approves the exact current head. Old-head approvals are stale.
6. GitHub branch protection remains the final mergeability authority; the
   controller conditionally squash-merges only when all current gates pass.

Do not ask the App, controller, Codex, or Hermes to approve, dismiss, resolve,
or bypass protection.

## 11. Status and Inspection

Important fields in the safe inspection projection include:

- run state, candidate/base/working branch, task/profile/registry digests;
- ordered transition timeline and last durable error;
- Codex attempts, session/model, artifact bindings, and outcome hashes;
- verifier process outcome and exact verified head;
- fresh reviews and normalized findings;
- side-effect intent/result, owned PR, polls, GitHub/Linear observations;
- trusted feedback lifecycle/conflicts/reply evidence and human approval;
- merge, source sync, cleanup, retry schedule, operator attention, and explicit
  operator-action provenance.

`operator_attention_events` is the normalized bounded projection. Each event
shows its envelope `schema_version`, stable key, sanitized reason/state,
payload/evidence digests, timestamps, and typed `allowed_actions`. Those actions
are display hints only; an authenticated state-changing command must
independently revalidate current run authority. Transport delivery state and
legacy local outbox fields are never exposed by `status` or `inspect`.
Automatic admission publishes one restart-stable `decide` attention event for
an active human-decision gate. Manual intervention and exhausted retry expose
only their typed valid actions. GitHub approval publishes no attention event:
the production driver remains its bounded polling authority. Repeated parked
cycles replay the same event key, and authority drift stays parked behind a
stable fail-closed reason.

Trusted-review manual stops expose one of the finite reasons
`trusted_review_topology_split_review`,
`trusted_review_topology_unsupported`, `trusted_review_feedback_drift`, or
`trusted_review_feedback_conflict` in the transition timeline and attention
event. The split reason means an inline root belonged to a `COMMENTED` review
while a separate trusted exact-head review supplied `CHANGES_REQUESTED`; the
controller does not combine those reviews. Reason fields contain no review body
or other actor-controlled prose.

`operator_actions` is a separate ordered projection for authenticated recovery
answers. It shows the allowlisted action, immutable requester identity, exact
attention/reason and transition binding, lifecycle/result, resulting
state/sequence, separate sanitized applied-evidence/outcome digests, and
the exact persisted retry eligibility plus received/validated/applied/observed
times. It never exposes the action or run
idempotency key, raw CLI arguments, paths, prose, or credentials. An entry is
human-action provenance; ordinary timeline transitions and external side
effects remain automatic/controller evidence. `controller retry` consumes this
journal; other recovery commands retain their dedicated boundaries. It is not a
generic state-mutation interface.

The persisted idempotency key is controller authority for an authenticated
recovery command, not a credential for an external service. Keep it run-scoped
and do not publish it unnecessarily.

## 12. Recovery Procedures

Use this order:

1. Stop concurrent worker processes if ownership is unclear.
2. Run `controller status` and `controller inspect` with the trusted requester.
3. Identify the current state, last transition/error, active lease/retry,
   pending side-effect intent, and actual GitHub/Linear/Git evidence.
4. Prefer `controller drive` when the state is a normal resumable or polling
   state.
5. Use one low-level command only when diagnosing a particular interrupted
   action.
6. Use a typed recovery (`recover-owned-push`, `accept-external-merge`, or
   `abandon`) only when its exact preconditions match.
7. Re-inspect after the action. Never edit SQLite, delete evidence, change a
   remote branch, or resolve a conflict merely to force the next state.

## 13. Logs, Artifacts, and SQLite

Codex JSONL stdout, stderr, semantic outcomes, verifier output, Git command
output, and schemas live in private per-attempt artifact directories. SQLite
stores private paths plus hashes/sizes and the sanitized evidence needed for
authorization and inspection.

LaunchAgent logs contain controller-sanitized stdout/stderr but remain private.
At every worker start, each current-user-owned, single-link mode-`0600` regular
stdout/stderr leaf is truncated when it has reached 8 MiB. Unsafe regular log
streams fail closed. The healthy worker emits no per-cycle log line, so an
indefinite process does not continuously grow these files. This startup bound
is the unattended policy; for retained history, rotate only while booted out,
retain an operator-chosen bounded number and size of generations, and recreate
mode-`0600` leaves before bootstrap.

Use the sensitive-output scanner before retaining or sharing evidence:

```sh
./scripts/scan-sensitive-output.sh /absolute/artifact/root /absolute/controller/state/root
```

Do not print credential files, use `sqlite3` to patch state, or copy private
artifacts into GitHub/Linear comments.

## 14. Backup and Upgrade

1. Boot out and confirm the worker is stopped.
2. Back up the private controller root and external credential files using an
   operator-controlled encrypted mechanism. Preserve permissions and do not
   commit the backup.
3. Build/install the new binary outside the repository.
4. Run `version`, `config validate`, `config inspect`, `config doctor`, and
   LaunchAgent doctor/validation.
5. Let the application open the database and apply ordered migrations. Never
   downgrade a database whose schema is newer than the binary supports.
6. Render/lint the new plist. Replace an old plist only after bootout and
   deliberate operator removal; `install` never overwrites it.
7. Bootstrap, observe status/logs, and inspect the resumed run.

There is no automatic backup command or migration rollback command.

## 15. Troubleshooting

| Symptom | Meaning and safe response |
| --- | --- |
| Configuration invalid | Run `config validate`; correct the strict JSON/reference/path error. Do not weaken validators or insert placeholder identities. |
| Unauthorized requester | Use the immutable GitHub `User` identity configured and frozen for the run. A matching login alone is insufficient. |
| Linear source drift | Inspect the source revision and changed task/branch/repository facts. Resolve the human/manual gate; do not overwrite the snapshot. |
| Repository/profile drift | Restore the exact frozen authority or deliberately terminate/recover through supported policy. Unrelated config edits must not retarget the run. |
| Codex interrupted | Restart with worker/`drive`; the controller inspects the started attempt and resumes only the persisted session when evidence permits. |
| Review findings loop | Inspect normalized finding source and persisted decisions. Every accepted clarification must be bound into fresh review; do not mark findings resolved in SQLite. |
| CI pending | Wait. The driver polls at the configured bound. |
| CI failed | A supported actionable failure becomes repair input; infrastructure/ambiguous failure may require attention. Inspect exact-head check evidence. |
| Stale approval | Ask I-Fan to review and approve the current head after all code changes; old approval cannot be reused. |
| Remote branch divergence | Stop and inspect ownership/PR/head. Use `recover-owned-push` only for its proven owned-PR repair case; never force an unrelated ref. |
| PR ownership conflict | Do not adopt by branch/title. Verify marker, body digest, IDs, head/base, and persisted intent; otherwise remain fail-closed. |
| Merge conflict or rejection | Read `awaiting_github_mergeability`/manual evidence and GitHub protection. Resolve repository/human conditions; do not bypass protection. |
| PR merged outside controller | Reconcile to durable manual intervention, then use `accept-external-merge` only if its exact checks and tree proof pass. |
| Linear completion pending | Verify external Linear automation/state. The controller only observes; keep driving or use one `reconcile-linear` diagnostic read. |
| Dirty source checkout | The controller leaves it untouched and emits attention. The operator decides how to clean/synchronize it, then retries cleanup if appropriate. |
| Cleanup partial failure | Inspect per-resource results and actual ownership. Retry `drive`/`cleanup`; only unfinished owned resources are retried. |
| Worker candidate scan incomplete | Inspect pagination and identity authority. The controller admits none from a truncated, duplicate, contradictory, or otherwise ambiguous scan. |
| Retry attention | Inspect failure class, phase, count, and reason. Terminal audit schedules do not authorize evidence deletion. |
| LaunchAgent not running | Run LaunchAgent `status`, inspect finite reason codes and private logs, correct binary/config/log permissions, then kickstart only when status recommends it. |
| LaunchAgent control timeout | Treat as unknown/attention. Run `status`; never assume success and issue an immediate duplicate control operation. |

When evidence remains unclear, stop external writes and preserve sanitized
artifacts for review. The correct fallback is `manual_intervention`, not manual
state surgery.
