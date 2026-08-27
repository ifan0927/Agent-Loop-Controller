# Operations

The canonical executable is `agentctl`; new launchd installations use
`io.agent-loop-controller.worker`. A host that still has `ifan-loop` or
`com.ifan.agent-loop-controller.worker` must use the explicit migration in
section 8. Do not manually install or bootstrap the neutral identity beside a
legacy LaunchAgent or LaunchDaemon.

## 1. Prerequisites

Production operation currently targets one local macOS user. Prepare:

- Go version declared by [`go.mod`](../go.mod);
- Git and either a clean local source checkout or one existing empty GitHub
  repository selected for Controller-managed initialization;
- `ripgrep` with PCRE2 support for repository and retained-evidence scans;
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
go build -o ./bin/agentctl ./cmd/agentctl
./bin/agentctl version
```

For LaunchAgent supervision, install a worker-owned non-symlink executable
outside any repository checkout. LaunchDaemon supervision also accepts a
root-owned executable. In both cases it must have an execute bit and no
group/world write bit. Example:

```sh
mkdir -p "$HOME/.local/bin"
go build -o "$HOME/.local/bin/agentctl" ./cmd/agentctl
chmod 755 "$HOME/.local/bin/agentctl"
```

Stop the worker before replacing the installed binary. Re-run configuration and
launchd supervisor checks after every upgrade.

## 3. Filesystem Layout

The default macOS controller root is:

```text
~/Library/Application Support/agent-loop-controller/
  controller.json       secret-free configuration, mode 0600
  controller.db         authoritative workflow state and evidence
  authority/            private configuration authority, mode 0700
    baseline.json       immutable pre-locator baseline binding, mode 0600
    locator.json        canonical config/database binding, mode 0600
    generations/        bounded raw recovery evidence, mode 0700
      <sha256>.json     immutable exact generation bytes, mode 0600
  secrets/              private directory, mode 0700
    linear-token        regular single-link file, mode 0600
  logs/                 private launchd worker logs, mode 0700
    worker.stdout.log   mode 0600
    worker.stderr.log   mode 0600
  repositories/         Controller-owned onboarding roots, mode 0700
    owner--repository/
      source/            managed checkout for empty-repository onboarding
      runs/              repository run/artifact root
      worktrees/         isolated worktree root
```

The GitHub App PEM is a separate protected regular file at the absolute path
named by configuration. Repository profiles separately name non-overlapping
source checkouts, run/artifact roots, and worktree roots. Do not place secrets
under any run root or repository.

`controller.db` and artifact directories are evidence, not editable operator
configuration. Never repair a run by editing SQLite or artifact JSON.

## 4. Configuration

Configuration version 5 is current. Versions 1 through 4 remain readable for
compatibility, but new installations should use `config init` and version 5.
The starter is intentionally incomplete and automatic admission is disabled.

The strict JSON document contains:

| Section | Responsibility |
| --- | --- |
| `controller` | Database path, Codex executable, local action timeout, and one complete immutable GitHub `User` operator identity |
| `linear` | Allowed GraphQL endpoint, credential source reference, team key, and bounded request settings |
| `github_app_profiles` | App/installation/repository identities, PEM reference, request bounds, and narrow write switches |
| `repositories` | Canonical owner/name, origins and local roots, base branch, verifier IDs, profile reference, and trusted actors |
| `automation.linear_todo_admission` | Disabled/enabled authority, exact workflow states, independent admission/delivery poll bounds, admission lease bounds, generic `heavy_capacity`, fixed requester, durable local event adapter mode, and credential reference |

Version 5 requires `controller.operator` with `login`, `database_id`, `node_id`,
and `type: "User"`. This controller-wide configured operator is a separate
configuration authority from `automation.linear_todo_admission.requester`, even
when both tuples identify the same human. It must also be present in every
current repository profile's allowed login and trusted immutable actor policy.
Issue content, Linear fields, and CLI repository selectors cannot supply or
override it. For a legacy version 1 through 4 baseline, the Controller derives
a migration operator only if one immutable actor is already trusted by every
repository profile. This migration-only authority does not retroactively change
legacy run/query authorization; it may authorize the exact current-schema
transition, after which version 5's explicit `controller.operator` is
authoritative. If legacy profiles have no common actor, startup and admission
convergence remain available, but configuration apply/history has no
controller-scope requester; the Controller never promotes an actor trusted by
only one repository.

The first production worker or management composition adopts an already-valid
live file exactly once as the baseline generation. It does not rewrite the
file. `config validate` and `config inspect` remain offline and never open
SQLite, create the `authority/` directory, reconcile an apply, or observe a
worker. Baseline preparation holds the private filesystem mutation authority
while it retains raw evidence and publishes an immutable private binding intent
for the exact live path, database path, database device/inode identity, digest,
size, and schema. It then
records the matching anchor in that database, publishes the private locator,
and finally settles the generation. After a crash, startup accepts either the
pre-locator intent or locator only after proving that exact private file
identity and its schema/binding on one query-only SQLite connection. The
adapter checks the actual SQLite VFS file descriptor, not another pathname
lookup, and repeats the VFS descriptor plus pathname check on every physical
connection and idle-pool reuse, and before plus after each transaction
boundary, direct query or effect, prepared-statement effect, and row-consumption
step. The check also revalidates current-user ownership, single-link state, and
mode `0600`. Writes and
forward migration are enabled only on that same connection without reopening
the pathname. Every later production store composition re-proves the persisted
database identity instead of using a path-only open. It never creates or
migrates an unproven target or follows a newly edited live
database path. The proof accepts database schemas from the configuration-
authority floor through the current binary's supported schema so a trusted
older store can be migrated normally; it rejects pre-authority and newer
unsupported schemas. After baseline, the locator and retained desired bytes bind the
canonical configuration to its existing database. An alternate configuration
path, database relocation, invalid live file, or out-of-band digest change is a
conflict; startup never re-baselines, follows an edited database path, adopts
drift, or rewrites the file before an explicit authorized recovery intent.

The Controller exposes normal typed-change and forward-rollback paths for
`controller.run_timeout` and the eight bounded
`automation.linear_todo_admission` policy fields. Use
`config draft open|show|set|validate|preview|apply|discard` and
`config rollback sources|open`; no command accepts
raw JSON, a candidate file, a generic key, a path, an identity, or a credential
reference. The single active draft is revision-CAS protected and bound to the
exact desired generation. Preview and apply expose only sanitized allowlisted
values and finite impacts. Rollback source discovery lists only retained,
superseded, safely committed schema-1-through-5 generations whose nine settings
project under current policy. Opening a rollback draft binds the exact source
and current desired authority, then materializes against current schema-5 base
bytes so every non-editable current authority remains unchanged. Apply delegates
to the existing generation/CAS service and never restarts the worker.

For one safely readable external edit, authorized `config status` includes a
bounded `recovery` offer containing the exact desired generation/digest,
authority version, and observed live digest. Pass those values unchanged to
`config recover restore`. The command durably accepts that exact occurrence,
restores only the retained current desired bytes, and returns the settled
receipt plus convergence. It creates no generation, preserves existing runs,
and never restarts the worker. Missing, unreadable, invalid, relocated, or
otherwise unsafe live files, missing desired raw evidence, changed authority or
live digest, and incomplete or ambiguous apply/recovery evidence remain
fail-closed. There is no force, adopt, import, merge, historical-restore, or raw
file recovery command.

A committed typed configuration change normally reports `restart_required`
because workers load configuration only at process startup. Use the existing
explicit supervised restart procedure, then `config status` until the desired,
effective, and live digests converge. Existing compatible runs continue under
their frozen authority and capacity reductions drain rather than revoke held
work. Raw generation files, baseline
binding, and locator contents are private recovery evidence: do not read, copy,
edit, prune, or use them as an operator API. Trusted reads reject either the
authority or generation ancestor if it is a symlink, has changed ownership, or
is not mode `0700`, even when the leaf itself still appears private. After each
bounded private-leaf read, the opened inode and current pathname are rechecked
for owner, mode `0600`, single-link state, and exact identity after the final
authority-directory sync. The leaf descriptor stays open across that sync.

Raw retention and pruning are serialized with configuration publication by a
flock on the stable filesystem-root inode; there is no user-replaceable
lock-file, configuration-parent, or authority-subtree lock pathname. Existing
identical publications and already-absent prune retries must re-sync their
parent directory before they can report durable success. That sync pins the
opened authority directory and revalidates its current pathname, owner, mode
`0700`, and inode before the still-open leaf or expected absence receives its
final pathname proof. First creation and every retry also fsync each authority
directory's parent entry while pinning both directory identities. Raw reads
preserve the same proof for both the outer authority and nested generation directory.
Exclusive raw,
binding, and locator publication uses the platform's atomic no-replace rename,
so a crash cannot leave the final leaf hard-linked to a temporary alias.
Restart cleanup removes interrupted temporary leaves and raw digest leaves
that never acquired a retained SQLite generation anchor. A competing apply
returns a safe conflict, while deferred pruning is retried by normal startup or
later apply reconciliation. Operators must not recreate or remove private
configuration evidence while a mutation may be active.

When an apply returns to a digest already reported by the current fresh worker,
the new generation can be recorded effective in that same response. No worker
restart is required merely because the digest appeared in an older generation.

The database does not provide a legacy admission fallback once it has schema
31 or newer. If configuration authority is absent, both manual run creation and
automatic reservation fail closed. Tests and offline fixtures must establish an
explicit ready test authority; operators must recover the canonical binding and
must not insert authority rows or run records manually.

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
affect the result. One nonterminal run owns each immutable repository binding;
an active repository candidate does not block an eligible idle repository.
Duplicate or contradictory identities and incomplete bounded scans still fail
closed for operator attention.

`heavy_capacity` is a positive integer from 1 through 32 and defaults to `2` in
versions 4 and 5. It bounds local Codex, verification, fresh review, and repair work;
external CI/Linear waits and human/manual waits do not consume it. Changes take
effect on worker restart. Lowering capacity drains without canceling current
holders. Version-3 `max_active_runs: 1` remains readable as capacity one for
upgrade compatibility; versions 4 and 5 reject that retired field.

`automation.linear_todo_admission.poll_interval` controls only idle Linear Todo
admission scans and remains bounded from `1m` through `1h` (`5m` in the starter
configuration). `delivery_poll_interval` independently controls active-run
GitHub and Linear delivery observations and the driver's immediate-action
guard. It is bounded from `30s` through `5m`, with a `30s` default. This fixed
30-second strategy is the smallest deterministic MVP policy: each active run
makes at most roughly two delivery attempts or observations per minute while
avoiding the five-minute ready-state latency caused by inheriting admission
cadence. This bound includes retryable unavailable side-effect attempts; each
attempt still performs its normal fresh authority validation and idempotency
checks. Pending CI remains pending and is reread at this cadence; durable wait
evidence and the repository's 20-minute default slow-CI threshold are unchanged.

Enabled version-3 configurations created before `delivery_poll_interval`
existed remain readable when it is omitted; the effective compatibility
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

These values must match the current or frozen target authority. Existing CLI
flags remain a compatibility authentication surface, but the application maps
them into the same typed requester and target-specific authorization contracts
used by other presentation adapters. Controller-wide queries additionally
require the exact version-5 configured operator. Unknown and unauthorized run
or repository detail/filter targets both return the same sanitized `not_found`
result.

## 6. Normal Operator Workflow

### Configure and validate

```sh
agentctl config init
# Complete controller.json and provision external credentials.
agentctl config validate
agentctl config inspect
agentctl config doctor
agentctl repository list <requester flags>
agentctl repository recheck owner/repository --request-id initial-readiness <requester flags>
agentctl repository enable owner/repository --request-id enable-after-readiness <requester flags>
```

Before enabling a live target, verify the selected repository identity, clean
base checkout, App installation/permissions, branch protection, required
checks, stale-approval dismissal, conversation resolution, and lack of bypass.

### Start automatic operation

Enable `automation.linear_todo_admission` only after validation. Then either run
the worker in the foreground:

```sh
agentctl controller worker
```

or install and supervise it with the launchd commands in section 8. The
worker receives no issue identifier. It resumes a nonterminal run before
scanning for a new Todo.

For one explicitly selected coding-ready issue, `controller run IFAN-123`
admits and drives that issue in the foreground. This is useful for deliberate
manual admission, recovery, and local controlled operation; it is not the
automatic Todo E2E trigger.

### Observe

The start/worker output exposes a run ID. Use requester-authorized queries:

```sh
agentctl controller status '<run-id>' <requester flags>
agentctl controller inspect '<run-id>' <requester flags>
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
agentctl controller drive '<run-id>' <requester flags>
```

Use `drive` when the prior foreground process ended because of a host restart,
signal, or the diagnostic `controller run`/`controller drive --max-runtime`,
and the persisted state remains a normal resumable state. Do not decompose the
workflow into recovery-only commands merely because the process stopped.

## 7. Command Reference

All examples omit `--config` when using the default path. Durations use Go
duration syntax such as `30s`, `15m`, and `24h`.

### Normal operator commands

### `onboarding existing open`, `empty open`, `preflight`, `preview`, `start`, `show`, `cancel`, and `resume`

Use this workflow to adopt one exact existing clean checkout without editing
configuration or SQLite by hand:

```sh
agentctl onboarding existing open \
  --request-id '<stable-id>' \
  --source-path '/absolute/canonical/checkout' \
  --repository owner/repository \
  --github-app-profile github-app-profile:repository \
  --base-branch main \
  --verifier-ids fixture-go-test \
  --linear-label-slug repository \
  <requester flags>
agentctl onboarding empty open \
  --request-id '<stable-id>' \
  --repository owner/repository \
  --github-app-profile github-app-profile:repository \
  --base-branch main \
  --verifier-ids fixture-go-test \
  --linear-label-slug repository \
  <requester flags>
agentctl onboarding preflight '<onboarding-id>' <requester flags>
agentctl onboarding preview '<onboarding-id>' <requester flags>
agentctl onboarding start '<onboarding-id>' \
  --preflight-digest '<digest>' --preview-digest '<digest>' \
  <requester flags>
agentctl onboarding show '<onboarding-id>' <requester flags>
```

Existing-checkout `open` retains the exact absolute path only in private
Controller authority; JSON projections and SQLite contain its digest. Empty-
repository `open` accepts no source path, remote URL, SSH setting, commit
metadata, or lifecycle intent. It derives one managed source beneath the
Controller repository root and retains that path only in the same private
closed input authority.

`preflight` is read-only. Existing checkout validation requires a canonical
owner-controlled non-symlink checkout at its exact Git top level, a clean
selected base branch, no in-progress Git operation, an exact matching
credential-free GitHub origin, and local HEAD equal to a bounded remote
base-head read. Empty-repository validation proves that the managed source and
roots do not exist or overlap another authority, proves the exact GitHub App
repository, and runs bounded `git ls-remote --refs` against the canonical
GitHub SSH remote; any advertised ref or unavailable read fails preflight.
Both kinds verify the App/profile, verifier IDs, configuration authority, and
exact Linear team/label state. `preview` names the selected kind, its exact
ordered effects, retained progress, possible worker restart, and final
`ready_disabled` state without exposing paths, URLs, credentials, SSH state,
or raw external payloads.

`start` requires both exact digests and reruns preflight before accepting the
receipt. The worker then resumes the durable saga before normal issue
admission. A configuration addition intentionally produces
`worker_restart_required`; restart the managed worker, then use `show`. Other
operator-correctable waits expose `resume`; fix the named external condition
and reuse that command. `cancel` is legal only before start. Reusing the same
open/start/resume request replays persisted authority rather than duplicating
effects. Empty-repository execution creates only an empty managed checkout and
one deterministic `Initialize repository` root commit, then publishes only
`refs/heads/<base>:refs/heads/<base>` without force. A restart or lost response
re-reads all remote refs before retry: the exact intended base is adopted, an
empty remote waits for explicit `resume`, divergence conflicts, and an
unobservable outcome requires the bounded runbook. Partial roots, managed
source, initial revision/base, an already-created Linear label, and an applied
configuration are retained and reconciled; there is no implicit destructive
rollback.

Completion is `ready_disabled`, even when all readiness dimensions are ready.
Inspect the repository projection and run `agentctl repository enable` with a
new stable request ID only after deliberate operator approval.

### `repository list`, `inspect`, `recheck`, `enable`, `disable`, and `remove`

**Purpose**

Inspect durable repository lifecycle/readiness and perform receipt-backed
readiness or lifecycle operations.

**Syntax**

```sh
agentctl repository list [--limit 50] [--cursor '<opaque>'] <requester flags>
agentctl repository inspect owner/repository <requester flags>
agentctl repository recheck owner/repository \
  --request-id '<stable-id>' <requester flags>
agentctl repository enable owner/repository \
  --request-id '<stable-id>' <requester flags>
agentctl repository disable owner/repository \
  --request-id '<stable-id>' <requester flags>
```

All commands accept `--config`. Requester flags must contain the complete
configured GitHub `User` identity. Mutations also require a caller-stable
`--request-id`; reuse the same value only to replay the same intended operation.
Output is JSON. Collection pagination is applied only after authorization, and
missing and unauthorized detail targets are indistinguishable.

`list` and `inspect` read only persisted projections and never contact Git,
GitHub, or Linear. `recheck` performs the eight configured read-only
observations and publishes only a complete authority-matched snapshot. It does
not fetch, checkout, create a branch, install an App, create a Linear label, or
run a verifier. After an upgrade, every adopted repository remains enabled for
compatibility but is `unknown` until its first successful recheck, so recheck
before expecting new admission. Enable only after the projection is `ready`;
disable remains legal while work is active and blocks only new
admission. Readiness remains separate from enabled/disabled intent, while any
profile or configuration authority change requires a fresh recheck.

If a recheck is interrupted, restart any managed command once to reconcile it
to `ambiguous`, then use a new request ID for a deliberate fresh observation.
Exact replay of a settled request returns its persisted receipt.

Repository retirement uses an explicit immutable draft workflow:

```sh
agentctl repository remove open owner/repository <requester flags>
agentctl repository remove show --draft-id <id> --revision 1 <requester flags>
agentctl repository remove validate --draft-id <id> --revision 1 <requester flags>
agentctl repository remove preview --draft-id <id> --revision 1 <requester flags>
agentctl repository remove apply --draft-id <id> --revision 1 \
  --preview-digest <digest> --incarnation-id <id> \
  --lifecycle-version <version> --profile-id <id> \
  --repository-binding-digest <digest> \
  --expected-generation-id <generation> --expected-digest <digest> \
  <requester flags>
agentctl repository remove discard --draft-id <id> --revision 1 <requester flags>
```

Disable the repository first. `validate` returns typed guard results and next
actions; `preview` is semantic and sanitized, names every preserved resource
category, and never exposes raw configuration, paths, URLs, credential
references, or secrets. Apply requires the exact preview, incarnation,
lifecycle, profile/binding, and desired-generation authorities. There is no
one-shot, force, raw-edit, or external-deletion form.

After apply, restart or reload the worker and use `show` until its receipt is
`observed`/`succeeded` with reason `retired`. The repository remains fenced and
visible as removal-pending until that worker reports the exact new
generation/digest. Current `list` and `inspect` then hide the retired target,
while its operation receipt and all historical run/audit evidence remain.
Local checkouts and managed directories, GitHub repositories/branches/PRs/App
profiles, Linear labels/issues, credentials/references, and artifacts are not
deleted. Removing the last repository is permitted only with automatic
admission disabled; the worker then has no repository admission source. A
configuration rollback does not restore the retired incarnation. Re-onboard
the same canonical name through the onboarding workflow to create a new
incarnation; never edit SQLite or configuration files to revive the tombstone.

**Related commands**

`config status`, `controller worker`, `controller run`.

### `version`

**Purpose**

Print the controller binary version.

**When to use**

Before an upgrade, compatibility check, or incident record.

**Syntax**

```sh
agentctl version
```

**Required arguments and flags**

None.

**Example**

```sh
agentctl version
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

Create an absent secret-free version 5 starter configuration and private
credential directory.

**When to use**

Once for a new operator installation.

**Syntax**

```sh
agentctl config init [--config <controller.json>]
```

**Required arguments and flags**

None; `--config` overrides the default path.

**Example**

```sh
agentctl config init
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
agentctl config path [--config <controller.json>]
```

**Required arguments and flags**

None.

**Example**

```sh
agentctl config path
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
agentctl config validate [--config <controller.json>]
```

**Required arguments and flags**

None.

**Example**

```sh
agentctl config validate --config /absolute/private/controller.json
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
agentctl config inspect [--config <controller.json>]
```

**Required arguments and flags**

None.

**Example**

```sh
agentctl config inspect
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
agentctl config doctor [--config <controller.json>]
```

**Required arguments and flags**

None.

**Example**

```sh
agentctl config doctor
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

### `config status`, `config draft`, `config rollback`, and `config recover`

**Purpose**

Observe managed configuration convergence and safely change routine typed
settings without editing or submitting raw `controller.json`.

**Syntax**

```sh
agentctl config status --requester <login> --requester-database-id <id> \
  --requester-node-id <node> --requester-type User
agentctl config draft open <complete-requester-flags>
agentctl config draft show --draft-id <id> --revision <n> <complete-requester-flags>
agentctl config draft set --draft-id <id> --revision <n> \
  --heavy-capacity 3 <complete-requester-flags>
agentctl config draft validate --draft-id <id> --revision <n> <complete-requester-flags>
agentctl config draft preview --draft-id <id> --revision <n> <complete-requester-flags>
agentctl config draft apply --draft-id <id> --revision <n> \
  --preview-digest <digest> --expected-generation-id <id> \
  --expected-digest <digest> <complete-requester-flags>
agentctl config draft discard --draft-id <id> --revision <n> <complete-requester-flags>
agentctl config rollback sources <complete-requester-flags>
agentctl config rollback open --source-generation-id <id> \
  --source-digest <digest> --expected-generation-id <id> \
  --expected-digest <digest> <complete-requester-flags>
agentctl config recover restore --expected-generation-id <id> \
  --expected-digest <digest> --expected-authority-version <version> \
  --observed-digest <digest> <complete-requester-flags>
```

`draft set` accepts exactly one of `--run-timeout`,
`--automatic-admission-enabled`, `--admission-poll-interval`,
`--delivery-poll-interval`, `--scheduler-lease-ttl`,
`--scheduler-lease-renewal-interval`, `--max-candidates`, `--max-pages`, or
`--heavy-capacity`. Every managed query and mutation requires the complete
configured GitHub User requester tuple. `--config` may select the canonical
configuration path.

Open resumes an existing active draft rather than rebasing or replacing it.
Each edit increments the revision and invalidates prior validation/preview.
Validation and preview are local and create no generation or receipt. Apply
recomputes validation and preview from retained base bytes, checks exact
generation/digest/preview authority, and returns the existing apply receipt plus
convergence projection. Exact retries return the same edit revision or apply
generation/receipt. Discard changes neither generation nor runtime state.

`rollback sources` returns the exact current desired generation authority and a
bounded sanitized eligible-source list. `rollback open` first reconciles any
incomplete apply, proves the live file still matches desired authority, verifies
the exact retained source, and opens revision 1 with source settings against the
current desired base. An exact retry returns the same rollback draft; an active
normal draft, another rollback source, stale source/base evidence, first-open
pruning, malformed history, unsupported policy, unresolved apply, or external
drift conflicts without replacement. Once creation has durably bound the typed
settings, an exact retry still returns that active draft even if the source raw
snapshot was pruned afterward. After open, use the ordinary `draft` commands.
Editing preserves immutable source identity. A real apply creates one new
current-schema generation whose parent is current desired and whose metadata
records the rollback source. A same-digest rollback records a source-bound
successful receipt and settled draft but creates no generation or restart need.
The historical raw snapshot may be pruned after draft creation because the
draft retains only typed scalar settings and sanitized source identity.

`config recover restore` accepts only the complete authority projected by the
current safe `config status` recovery offer. It reauthorizes and rechecks the
live digest under the same Controller-wide mutation authority as apply, then
restores the exact retained desired bytes. Exact response-loss retries return
the same recovery intent and receipt. A concurrent third edit is restored when
safe and leaves an ambiguous fenced recovery; do not retry with substituted
digests or edit the private stage.

If the desired baseline is older than schema 5, upgrade it through the bounded
legacy-to-current transition before opening a draft. Preserve an applying or
ambiguous draft and the private `authority/` tree; do not edit SQLite or force a
second apply. `config validate` and `config inspect` remain separate SQLite-free
offline commands.

**Possible durable stop states**

An open draft may remain until explicitly applied or discarded. An ambiguous
apply or recovery remains non-editable and requires a later dedicated recovery
capability; safe-drift restore does not settle either ambiguity.
`restart_required` is resolved only by the existing operator-owned worker
restart and subsequent matching heartbeat.

**Safety notes**

Draft IDs, revisions, digests, and receipts are compare-and-swap evidence, not
bearer authorization. Never share private configuration paths or authority
files to troubleshoot a sanitized conflict.

**Related commands**

`config validate`, `config inspect`, `config rollback sources`, `config recover restore`,
`controller launchagent status`,
`controller launchdaemon status`.

### `controller worker`

**Purpose**

Run automatic Linear Todo admission and the production driver.

**When to use**

For normal automatic operation, run directly or under one supported launchd
supervisor.

**Syntax**

```sh
agentctl controller worker [--config <controller.json>] [--once]
```

**Required arguments and flags**

No positional argument. `--once` performs one resume or scan/dispatch cycle.
Normal worker operation has no process-lifetime expiry; operation-specific
network, process, verification, and control timeouts remain bounded.

**Example**

```sh
agentctl controller worker --once
```

**What it does**

Validates automation authority and credential topology, reconciles every
nonterminal run and its repository slot, execution lease, and heavy permit,
then resumes existing runnable work before scanning for new Todos. Heavy work
remains bounded by `heavy_capacity`; one additional supervisor slot preserves
admission and due external polls while all heavy supervisors are occupied. A
short versioned admission lease serializes scan, authoritative reread, atomic
reservation, and Linear mutation handoff; it is released before local heavy
work. Capacity exhaustion performs no Linear mutation. Unknown CAS results,
duplicate repository ownership, and globally ambiguous process ownership fail
closed as attention. External and human waits retain the repository slot but
release the heavy permit after process-stop proof. Attention parks only the
affected authority unless evidence is controller-global. External polls move
their durable `runnable_since` by `delivery_poll_interval`; the earliest such
deadline wakes the worker even when the admission scan cadence is slower.
Human waits are not repeatedly driven. Manual `continue`, `controller run`, and
`controller drive` use the same heavy permit authority. A direct driver and the
automatic worker are mutually excluded by the database-directory process lock,
which also fences restart-only permit adoption. While holding that lock,
`linear start` and `controller run` act as the sole manual supervisor and
publish their own process-bound heartbeat before checking new-admission
convergence; heartbeat failure cancels their work and fails the command. The
automatic worker reports bounded worker and queue-decision evidence;
`status` is `running`, `driving`, `parked`, or `stopping`, and a stopping result
includes `previous_status`. The active supervisor atomically replaces the private
`<controller-config>.worker-status.json` heartbeat after initialization, on
each activity transition, and every fixed 15 seconds even while quiet or
parked. The current schema binds the worker instance, PID, OS process-start
identity, binary build identity, exact loaded configuration digest, sanitized
activity, last completed cycle outcome, last queue-decision reason,
worker-owned next admission evaluation, and observation time. It stores no configuration
generation. Each publication uses a bounded exclusive mode-`0600` temporary
leaf, complete write and fsync, and atomic replacement; routine heartbeats do
not append logs or SQLite audit rows.

The controller-scope runtime observation classifies this evidence as `fresh`,
`stale`, `offline`, `unknown`, or `conflict`, separately from worker activity.
Age through 45 seconds is fresh only when the live PID and exact process-start
identity match; age over 45 seconds is stale. Missing processes, PID reuse,
unsafe or invalid evidence, unavailable identity, and future timestamps fail
closed to finite reasons. A schema-v2 heartbeat remains current
liveness/activity evidence and projects the newer cadence fields as unknown. A
schema-v1 activity snapshot remains recognizable legacy evidence but can never
satisfy heartbeat freshness. LaunchAgent and
LaunchDaemon status use this same observation and additionally require the
heartbeat PID to match launchd's observed service PID. Their JSON does not
expose PID, process-start identity, UID, heartbeat path, or raw file errors.

**Possible durable stop states**

Disabled policy, `--once`, SIGINT, or SIGTERM. Human decision, manual
intervention, retry attention, incomplete scan, no candidate, and terminal run are
durable outcomes observed by the continuing worker, not process expiry.

**Safety notes**

Never pass an issue identifier. `--once` intentionally uses one dispatch so it
cannot return while sibling dispatch goroutines are still mutating state.
SIGINT/SIGTERM cancels active drivers and their children, performs bounded lease
cleanup, closes SQLite, and emits a sanitized `stopped: canceled` result. It
does not rewrite runs as failed or abandoned. An unexpected failure exits
nonzero so launchd can restart and resume from persisted state without duplicate
admission. Failure to create, encode, write, synchronize, close, or atomically
publish the canonical heartbeat also cancels and joins worker dispatch, then
exits nonzero; there is no heartbeat-degraded mode that continues delivery.
A fresh heartbeat proves local runtime liveness and its loaded digest only.
Controller convergence additionally requires exact live/desired equality, no
unresolved apply, a durable effective observation for the current desired
generation, and the matching fresh identity-verified heartbeat. A safely
committed desired digest with a fresh worker still reporting the prior digest
is `restart_required`; use the existing LaunchAgent/LaunchDaemon stop/start or
kickstart procedure in this runbook. A fresh matching digest awaiting durable
CAS settlement is `starting`, never another restart request. Drift or ambiguous
evidence is `conflict` and has no force-overwrite procedure in this milestone.
Automatic and manual new admission remain fenced until the projection is
`ready`; compatible existing runs continue under frozen authority.

After onboarding and normal admission have received their opportunity, each
worker cycle also advances at most one bounded Controller-wide integrity family
batch. This maintenance reads SQLite only, consumes no heavy permit, commits
restart progress transactionally, and never changes workflow readiness or
external effects. Recoverable integrity degradation stays in its own projection
and does not stop existing delivery. The implemented interface is the
authorization-first application summary/detail contract; there is no new
public CLI command, explicit recheck operation, activity-feed event, raw-row
export, repair action, or manual SQL recovery procedure in this slice.

**Related commands**

`controller status`, `controller inspect`, `controller drive`, launchd
commands.

### `controller run`

**Purpose**

Admit one explicit Linear issue and drive its persisted workflow.

**When to use**

For deliberate manual admission or controlled recovery; not for automatic Todo
acceptance tests.

**Syntax**

```sh
agentctl controller run <IFAN-issue> [--config <file>] <requester flags> \
  [--poll-interval <duration>] [--max-immediate-actions <n>] \
  [--max-runtime <duration>]
```

**Required arguments and flags**

One issue identifier and complete requester identity. Poll defaults to `30s`,
immediate actions to `32`, and runtime to `24h` (maximum `168h`).

**Example**

```sh
agentctl controller run IFAN-123 \
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
agentctl controller drive <run-id> [--config <file>] <requester flags> \
  [--poll-interval <duration>] [--max-immediate-actions <n>] \
  [--max-runtime <duration>]
```

**Required arguments and flags**

Run ID and complete requester identity; policy bounds match `controller run`.

**Example**

```sh
agentctl controller drive '<run-id>' <requester flags>
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
agentctl controller status <run-id> [--config <file>] <requester flags>
```

**Required arguments and flags**

Run ID and complete requester identity.

**Example**

```sh
agentctl controller status '<run-id>' <requester flags>
```

**What it does**

Reads SQLite only and returns run, timeline, attempts, evidence, owned resources,
external observations, and safe recovery fields. `pull_request_aggregate` is
the explicitly mutable controller aggregate. Immutable creation-journal and
GitHub read history appears under `pull_request_observations`; the separate
`pull_request` field is the effective status derived from typed terminal
evidence. The GitHub portion is limited to the deterministic latest 100 rows;
`pull_request_observations_total` reports the durable count and
`pull_request_observations_truncated` reports whether older rows were omitted.
All rows are still integrity-checked, and effective feedback retains its latest
matching candidate even when that row predates the output window. Trusted
feedback similarly separates its initial snapshot and
controller lifecycle from `effective_thread_status`, selected from the latest
repository-, PR-, and strict-thread-matching immutable observation. An
effective `unknown` or `conflict` means the persisted evidence is missing or
inconsistent and must not be interpreted as open, closed, merged, resolved, or
unresolved. In particular, a missing or invalid persisted repository binding
projects `unknown`; a GitHub observation from a different repository owner,
name, or database ID projects `conflict`. A merge result also requires the
stored PR aggregate to have complete identity and to match the run's branch,
base branch, candidate head, base SHA, and ownership key. For resolved
feedback, an earlier unresolved GitHub read remains historical, while an
equal-time or later unresolved read projects `conflict`. A missing GitHub
observation time, a feedback row detached from the run/PR authority, or an
invalid feedback identity, lifecycle, or timestamp also projects `conflict`.
Inspection rejects GitHub evidence when its SQL head, repository ID, or
canonical observation time disagrees with its digest-bound JSON; equal
observation instants retain insertion order. Merge evidence likewise requires
full lowercase hexadecimal pre-merge, base, and merge commit SHAs.

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
agentctl controller inspect <run-id> [--config <file>] <requester flags>
```

**Required arguments and flags**

Run ID and complete requester identity.

**Example**

```sh
agentctl controller inspect '<run-id>' <requester flags>
```

**What it does**

Currently returns the same detailed projection as `status`; its name signals
diagnostic intent. Snapshot/effective PR and feedback semantics are identical
to `status`; neither command contacts GitHub or changes persisted evidence.

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
agentctl controller continue <run-id> [--config <file>] <requester flags> \
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
agentctl controller continue '<run-id>' <requester flags> \
  --repository owner/repository \
  --expected-state awaiting_human_decision \
  --idempotency-key '<persisted-key>' --decision /private/decision.json
```

**What it does**

Revalidates Linear, binds the selected offered choice to its exact originating
outcome, persists the decision, and advances/resumes the local controller.
The command takes the database-directory process lock for its complete lifetime,
fences permit adoption with a unique `manual:<run-id>:<nonce>` owner, and
reconciles any interrupted
managed attempt before adopting its permit. It therefore fails closed while the
automatic worker or another direct controller process owns that lock.
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
agentctl controller <command> <run-id> [--config <file>] <requester flags> \
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
agentctl controller retry '<run-id>' [--config <file>] <requester flags>
```

**Required arguments and flags**

The run ID and all requester identity flags. Repository, state, transition
sequence, Linear revision, idempotency key, retry reason, and local ownership
come only from persisted controller authority; this command does not accept
caller-supplied replacements.

**Example**

```sh
agentctl controller retry '<run-id>' \
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
agentctl controller recover-ci-wait '<run-id>' [--config <file>] \
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
agentctl controller continue '<run-id>' <requester flags> \
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
agentctl controller push '<run-id>' <requester flags> \
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
agentctl controller recover-owned-push '<run-id>' <requester flags> \
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
agentctl controller open-pr '<run-id>' <requester flags> \
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
agentctl controller reconcile '<run-id>' <requester flags> \
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
agentctl controller merge '<run-id>' <requester flags> \
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
agentctl controller accept-external-merge '<run-id>' <requester flags> \
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
agentctl controller reconcile-linear '<run-id>' <requester flags> \
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
agentctl controller cleanup '<run-id>' <requester flags> \
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
releasing the repository slot even when cleanup leaves residue.

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
agentctl controller abandon '<run-id>' <requester flags>
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
agentctl linear start <IFAN-issue> [--config <file>] <requester flags>
agentctl controller start <IFAN-issue> [--config <file>] <requester flags>
```

**Required arguments and flags**

One IFAN identifier and complete requester identity. The two forms route to the
same implementation.

**Example**

```sh
agentctl linear start IFAN-123 <requester flags>
```

**What it does**

Reads/admit the authoritative Linear issue and invokes the bounded local
controller. It takes the same database-directory process lock as the automatic
worker and reconciles an interrupted existing run before resuming it. It does
not construct the long-lived GitHub delivery driver.

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
agentctl github-read [--config <file>] --run-id <run-id> \
  <requester flags> --repository <owner/name> \
  --expected-state <state> --idempotency-key <persisted-key> \
  --pr <number> --expected-head <sha>
```

**Required arguments and flags**

Complete run/requester/repository/state/idempotency authority, positive PR
number, and exact expected head.

**Example**

```sh
agentctl github-read --run-id '<run-id>' <requester flags> \
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

## 8. Automatic Worker and launchd Supervision

The embedded LaunchAgent runs exactly:

```text
<absolute-binary> controller worker --config <absolute-config>
```

It uses label `io.agent-loop-controller.worker`, `RunAtLoad`, restart only
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
--binary <absolute-installed-binary>   default /usr/local/bin/agentctl
--legacy-binary <absolute-legacy>      default /usr/local/bin/ifan-loop; migration/rollback only
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
agentctl controller launchagent doctor [common flags]
```

**Required arguments and flags**

No positional arguments; supply the actual installed binary and configuration.

**Example**

```sh
agentctl controller launchagent doctor --binary "$HOME/.local/bin/agentctl"
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
agentctl controller launchagent validate [common flags]
```

**Required arguments and flags**

No positional arguments.

**Example**

```sh
agentctl controller launchagent validate --binary "$HOME/.local/bin/agentctl"
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
agentctl controller launchagent build [common flags]
```

**Required arguments and flags**

No positional arguments.

**Example**

```sh
agentctl controller launchagent build --binary "$HOME/.local/bin/agentctl" > /tmp/worker.plist
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
agentctl controller launchagent install [common flags]
```

**Required arguments and flags**

No positional arguments.

**Example**

```sh
agentctl controller launchagent install --binary "$HOME/.local/bin/agentctl"
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
agentctl controller launchagent plist-validate [common flags]
```

**Required arguments and flags**

No positional arguments.

**Example**

```sh
agentctl controller launchagent plist-validate --binary "$HOME/.local/bin/agentctl"
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
agentctl controller launchagent bootstrap [common flags]
```

**Required arguments and flags**

No positional arguments.

**Example**

```sh
agentctl controller launchagent bootstrap --binary "$HOME/.local/bin/agentctl"
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
agentctl controller launchagent status [common flags]
```

**Required arguments and flags**

No positional arguments.

**Example**

```sh
agentctl controller launchagent status --binary "$HOME/.local/bin/agentctl"
```

**What it does**

Returns a finite observed state/outcome/next action without exposing raw
`launchctl` output. While the service is running it also returns the canonical
runtime liveness, sanitized activity, finite reason, heartbeat age/observation
time, worker/build identity, and loaded configuration digest. The projection
requires the configured controller operator authority and checks both the
heartbeat process identity and launchd's observed PID. `parked` may therefore
be `fresh`; workload activity is not runtime readiness.

**Possible durable stop states**

No controller run state change.

Routine application queries are currently library contracts for the planned
local operator adapter; they do not add public CLI routes. They read persisted
queue, run, attention, repository, onboarding, and configuration evidence only.
They never trigger Linear, GitHub, Git, verifier, credential, or readiness
refreshes, and the settings convergence path performs no reconciliation or
authority write.

Activity list/detail and operation-receipt history are also application-only
query contracts; no new public CLI route is added. Activity defaults to 50 and
rejects limits above 100. Receipt history defaults to 25 and rejects limits
above 100. Both authenticate the complete configured operator before applying
scope and filters, counting, ordering, pagination, or cursor construction.
Continuation cursors are opaque and invalid after scope or filter drift.
Queries never backfill, reconcile runtime observations, inspect files, contact
external systems, acknowledge attention, or change workflow state.

The automatic worker performs at most one bounded SQLite-only legacy activity
backfill batch per dispatch opportunity before normal onboarding/admission
work, without taking a local-heavy-work permit. Progress resumes after restart;
current indexing and backfill can interleave through deterministic identities.
Coverage reports a finite state and reason, the proven/indexed boundary when
available, progress counts, runtime freshness when available, and fixed legacy
limitations. `complete` means complete only within reconstructable persisted
evidence. A source conflict stops that source and preserves completed progress;
an indexing failure degrades coverage and does not authorize or stop ordinary
Controller workflow.
The next successful indexing cycle clears a transient degradation marker.
Known fixed legacy gaps remain listed even when coverage is `complete` within
the reconstructable boundary: non-persisted worker/runtime and repository
intent transitions, partially reconstructable attention resolution, and
admission-capacity history that is not part of the backfill set.

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
agentctl controller launchagent kickstart [common flags]
```

**Required arguments and flags**

No positional arguments.

**Example**

```sh
agentctl controller launchagent kickstart --binary "$HOME/.local/bin/agentctl"
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
agentctl controller launchagent bootout [common flags]
```

**Required arguments and flags**

No positional arguments.

**Example**

```sh
agentctl controller launchagent bootout --binary "$HOME/.local/bin/agentctl"
```

**What it does**

Stops the exact label at most once; an absent service is idempotent. After an
accepted bootout request, it reconciles the exact label with read-only status
observations inside the original bounded control window. A delayed
disappearance is reported as `stopped`. If absence is not observed before that
window expires, the result is `attention_required` with
`bootout_observation_timeout`, the last sanitized observed state, and
`timed_out: true`; the controller never sends a duplicate bootout request.

**Possible durable stop states**

The active run remains persisted and resumable.

**Safety notes**

Confirm with `status` before replacing binaries, logs, or plist files. An
unknown observation or timeout authorizes only another read-only status check,
not another automatic control operation.

**Related commands**

`status`, `bootstrap`, `controller drive`.

### Legacy executable and launchd identity migration

The repository-controlled migration is available on both
`controller launchagent` and `controller launchdaemon` as
`migration-status`, `migrate`, and `rollback`. It changes only executable/plist
supervision identity: the Controller database, configuration, credentials,
worktrees, artifacts, workflow state, leases, and evidence remain in place.

`migration-status` is read-only and classifies the selected supervisor as one
of `legacy_running`, `legacy_installed_stopped`, `neutral_running`,
`neutral_only`, `neither_installed`, `both_configured`,
`interrupted_migration`, `rollback_interrupted`, `rolled_back`, or an explicit
conflict/attention state. It observes both labels in both supported supervisor
domains; a stopped plist still counts as configured. Unknown state and any
dual-worker topology fail closed.

Before `migrate`, build and validate a separate neutral binary. Do not replace
or delete the legacy binary: its exact path must still match the legacy plist
and remains the bounded rollback executable.

For a per-login LaunchAgent:

```sh
NEW_BIN="$HOME/.local/bin/agentctl"
LEGACY_BIN="$HOME/.local/bin/ifan-loop"
CONFIG="$HOME/Library/Application Support/agent-loop-controller/controller.json"

"$NEW_BIN" controller launchagent migration-status \
  --binary "$NEW_BIN" --legacy-binary "$LEGACY_BIN" --config "$CONFIG"
"$NEW_BIN" controller launchagent migrate \
  --binary "$NEW_BIN" --legacy-binary "$LEGACY_BIN" --config "$CONFIG"
"$NEW_BIN" controller launchagent status \
  --binary "$NEW_BIN" --legacy-binary "$LEGACY_BIN" --config "$CONFIG"
```

For a headless LaunchDaemon, run status as the worker user and only the bounded
mutation under root:

```sh
NEW_BIN="$HOME/.local/bin/agentctl"
LEGACY_BIN="$HOME/.local/bin/ifan-loop"
CONFIG="$HOME/Library/Application Support/agent-loop-controller/controller.json"
WORKER_USER="$(id -un)"

"$NEW_BIN" controller launchdaemon migration-status \
  --binary "$NEW_BIN" --legacy-binary "$LEGACY_BIN" \
  --config "$CONFIG" --user "$WORKER_USER"
sudo "$NEW_BIN" controller launchdaemon migrate \
  --binary "$NEW_BIN" --legacy-binary "$LEGACY_BIN" \
  --config "$CONFIG" --user "$WORKER_USER"
"$NEW_BIN" controller launchdaemon status \
  --binary "$NEW_BIN" --legacy-binary "$LEGACY_BIN" \
  --config "$CONFIG" --user "$WORKER_USER"
```

`migrate` boots out the exact legacy label at most once, observes that label as
absent, and takes the authenticated `worker.lock` before changing any plist.
It moves the legacy plist to `<legacy-plist>.agentctl-rollback`, installs or
restores the exact neutral plist, releases the fence, and bootstraps the neutral
label. If interruption occurs after legacy stop, plist backup, neutral install,
or bootstrap, rerun `migration-status`; only rerun `migrate` when it returns
that next safe action. A missing plist or stale heartbeat never proves process
absence.

Rollback uses the same flags:

```sh
sudo "$NEW_BIN" controller launchdaemon rollback \
  --binary "$NEW_BIN" --legacy-binary "$LEGACY_BIN" \
  --config "$CONFIG" --user "$WORKER_USER"
```

Omit `sudo` and `--user` for LaunchAgent rollback. The command first boots out
and proves absence of the neutral label, obtains the worker fence, moves the
neutral plist to `<neutral-plist>.agentctl-disabled`, restores the legacy plist,
and only then bootstraps the legacy service. An already rolled-back topology is
idempotent. Never invoke the old binary's bootstrap command directly after a
neutral plist has been prepared; the old implementation cannot know the new
label.

No `ifan-loop` executable alias is installed. Retain the actual legacy binary
and `.agentctl-rollback` plist until all of these are true: neutral status
returns `worker_identity_verified: true`, proving the current launchd PID is
bound to the private process-start identity; one deliberate
neutral service restart or reboot has adopted the same SQLite state; every run
that was nonterminal during migration has reached a newly observed durable stop
with no authenticated pre-migration managed process left; and the operator no
longer requires immediate rollback. Only then may the operator remove those two
exact legacy files. Legacy managed-launch and review-reply marker reads remain
for in-flight/local and external idempotency evidence; that compatibility is
independent of the executable file's removal.

### Headless system LaunchDaemon

Use `controller launchdaemon` when the Mac must restore the worker after boot
without a graphical login. It supports the same
`build|render|install|doctor|validate|plist-validate|bootstrap|kickstart|status|bootout|migration-status|migrate|rollback`
operations as the LaunchAgent surface, but the domain is fixed to `system`.
There is no `--domain` flag and `user/<uid>` is never accepted.

All LaunchDaemon commands share:

```text
--binary <absolute-installed-binary>   default /usr/local/bin/agentctl
--legacy-binary <absolute-legacy>      default /usr/local/bin/ifan-loop; migration/rollback only
--config <absolute-controller.json>    default below the worker user's home
--plist <absolute-plist>               default /Library/LaunchDaemons/io.agent-loop-controller.worker.plist
--user <account>                       worker account; required under root
--working-directory <absolute-dir>     default worker user's home
--timeout <duration>                   default 15s, maximum 2m
```

The exact plist is root-owned mode `0600`, loaded into `system`, and pins the
non-root `UserName`, absolute `WorkingDirectory`, and non-secret `HOME`. It
contains no token, authorization header, credential reference, shell, issue, or
branch. `doctor` runs as the worker user and verifies the executable,
configuration, database/log parents, Codex auth file, GitHub App private keys,
and file-backed Linear credential. Environment-backed Linear credentials are
not supported for a pre-login LaunchDaemon. `install`, `bootstrap`, `kickstart`,
and `bootout` require root; the worker itself refuses effective UID 0.

#### Migrate from LaunchAgent

Set exact paths first and retain the `.rollback` file until the reboot and
rollback gates have both passed:

```sh
BIN="$HOME/.local/bin/agentctl"
CONFIG="$HOME/Library/Application Support/agent-loop-controller/controller.json"
WORKER_USER="$(id -un)"
AGENT_PLIST="$HOME/Library/LaunchAgents/io.agent-loop-controller.worker.plist"
DAEMON_PLIST="/Library/LaunchDaemons/io.agent-loop-controller.worker.plist"

"$BIN" controller launchagent bootout --binary "$BIN" --config "$CONFIG" --plist "$AGENT_PLIST"
"$BIN" controller launchagent status --binary "$BIN" --config "$CONFIG" --plist "$AGENT_PLIST"
mv "$AGENT_PLIST" "$AGENT_PLIST.rollback"

"$BIN" controller launchdaemon doctor --binary "$BIN" --config "$CONFIG" --user "$WORKER_USER"
"$BIN" controller launchdaemon validate --binary "$BIN" --config "$CONFIG" --user "$WORKER_USER"
"$BIN" controller launchdaemon render --binary "$BIN" --config "$CONFIG" --user "$WORKER_USER" | plutil -lint -
sudo "$BIN" controller launchdaemon install --binary "$BIN" --config "$CONFIG" --user "$WORKER_USER"
sudo "$BIN" controller launchdaemon plist-validate --binary "$BIN" --config "$CONFIG" --user "$WORKER_USER"
sudo "$BIN" controller launchdaemon bootstrap --binary "$BIN" --config "$CONFIG" --user "$WORKER_USER"
"$BIN" controller launchdaemon status --binary "$BIN" --config "$CONFIG" --user "$WORKER_USER"
```

Stop if either status is unknown, if the old service is not absent, if the
opposite plist still has its `.plist` name, or if any finite reason code is
reported. Bootstrap checks both installed and loaded opposite-supervisor state;
the process-lifetime worker lock remains a final fence against an accidental
second process before scheduler runtime construction.

For the headless gate, use an authenticated `fdesetup authrestart`, reconnect by
SSH without logging into the desktop, and prove the system service PID belongs
to `WORKER_USER`. Confirm exactly one worker process, the sole
`linear_todo_admission` scheduler-lease namespace, and no admitted fixture while
the fixture remains in Triage. Cycle assignment and the move to Todo are later
human admission actions, not part of supervisor recovery.

#### Roll back to LaunchAgent

Rollback first removes system supervision, verifies absence, preserves the
daemon plist under a non-`.plist` name, and only then restores the LaunchAgent:

```sh
sudo "$BIN" controller launchdaemon bootout --binary "$BIN" --config "$CONFIG" --user "$WORKER_USER"
"$BIN" controller launchdaemon status --binary "$BIN" --config "$CONFIG" --user "$WORKER_USER"
sudo mv "$DAEMON_PLIST" "$DAEMON_PLIST.rollback"
mv "$AGENT_PLIST.rollback" "$AGENT_PLIST"
"$BIN" controller launchagent plist-validate --binary "$BIN" --config "$CONFIG" --plist "$AGENT_PLIST"
"$BIN" controller launchagent bootstrap --binary "$BIN" --config "$CONFIG" --plist "$AGENT_PLIST"
"$BIN" controller launchagent status --binary "$BIN" --config "$CONFIG" --plist "$AGENT_PLIST"
```

The final bootstrap requires an existing `gui/<uid>` domain. If rollback is
prepared over headless SSH, stop after restoring and validating the LaunchAgent
plist, then bootstrap after the next graphical login. Never rename the opposite
backup to `.plist` while the current service is loaded.

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
3. The configured human reviewer may submit an exact-head inline root
   `CHANGES_REQUESTED` review. The controller authenticates it, repairs,
   re-verifies/re-reviews, pushes a new head, and posts one fixed reply.
4. The configured human reviewer reviews the repair and resolves the
   conversation when satisfied. The controller never resolves it.
5. The configured human reviewer approves the exact current head. Old-head
   approvals are stale.
6. GitHub branch protection remains the final mergeability authority; the
   controller conditionally squash-merges only when all current gates pass.

Do not ask the App, controller, Codex, or Hermes to approve, dismiss, resolve,
or bypass protection.

## 11. Status and Inspection

Controller-wide integrity summary and affected-scope detail are available to
presentation adapters through the typed application query contract after exact
configured-operator authentication. The summary is `unknown` before the first
complete observation and immediately after a registered source generation
advances. Detail is bounded to 50 findings by default and 100 maximum, uses an
observation- and authorization-bound cursor, and exposes no SQL, table or row
identity, path, URL, credential, payload, log, or arbitrary error. Existing
`controller status` and `controller inspect` JSON remain unchanged. Explicit
receipt-backed recheck and TUI rendering remain follow-up work.

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

`operator_actions` remains the ordered compatibility projection for
authenticated recovery answers. It shows the allowlisted action, immutable
requester identity, exact attention/reason and transition binding,
lifecycle/result, resulting
state/sequence, separate sanitized applied-evidence/outcome digests, and
the exact persisted retry eligibility plus received/validated/applied/observed
times. It never exposes the action or run
idempotency key, raw CLI arguments, paths, prose, or credentials. An entry is
human-action provenance; ordinary timeline transitions and external side
effects remain automatic/controller evidence. Decision, retry, abandon,
CI-wait recovery, owned-push recovery, and external-merge acceptance all bind
this action-specific journal to the common scope-neutral operation receipt
before controller mutation. The presentation-independent legal-action and
single-receipt application queries are not new CLI mutation commands. The
bounded operation-history application query returns these same sanitized
receipt fields ordered by immutable `accepted_at` and operation identity. It
does not expose authority keys, operation anchors, raw requests, command
arguments, process/session identities, run idempotency keys, or private
evidence. Receipt phase/outcome may advance while browsing without changing
collection order.

Every execution still enters its dedicated revalidation, ownership, exact-head, CAS,
lease, and reconciliation boundary; there is no generic state-mutation
interface.

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

Worker logs contain controller-sanitized stdout/stderr but remain private.
At every worker start, each current-user-owned, single-link mode-`0600` regular
stdout/stderr leaf is truncated when it has reached 8 MiB. Unsafe regular log
streams fail closed. The healthy worker emits no per-cycle log line, so an
indefinite process does not continuously grow these files. This startup bound
is the unattended policy; for retained history, rotate only while booted out,
retain an operator-chosen bounded number and size of generations, and recreate
mode-`0600` leaves before bootstrap.

Use the sensitive-output scanner before retaining or sharing evidence:

```sh
./scripts/scan-sensitive-output.sh "$ARTIFACT_ROOT" "$CONTROLLER_DB" \
  "$WORKER_STDOUT_LOG" "$WORKER_STDERR_LOG"
"$BIN" config doctor --config "$CONFIG"
```

The scanner accepts only output and non-secret state evidence. It reports fixed
reason codes without matched bytes, lines, file names, or paths. Never pass the
controller root, credential directory, or credential leaf to it; `config
doctor` checks the intentional credential source's secure topology separately.
Do not print credential files, use `sqlite3` to patch state, or copy private
artifacts into GitHub/Linear comments.

## 14. Backup and Upgrade

1. Boot out and confirm the worker is stopped.
2. Back up the private controller root and external credential files using an
   operator-controlled encrypted mechanism. Preserve permissions and do not
   commit the backup.
3. Build/install the new binary outside the repository.
4. Run `version`, `config validate`, `config inspect`, `config doctor`, and
   selected supervisor doctor/validation.
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
| Configuration restart required | Use the existing supervised worker restart procedure, then wait for a fresh matching heartbeat and durable effective observation. Do not edit SQLite or the private locator. |
| Safely readable configuration drift | Use the exact authorized `recovery` offer from `config status` with `config recover restore`. Do not substitute a digest, copy raw evidence manually, adopt the external bytes, or request a worker restart as part of recovery. |
| Unsafe configuration drift or ambiguous apply/recovery | Stop new admission and preserve `authority/` evidence. Do not rebaseline, edit SQLite, change the locator, remove a private stage, or force overwrite; this recovery slice intentionally remains fail-closed. |
| Unauthorized requester | Use the immutable GitHub `User` identity configured and frozen for the run. A matching login alone is insufficient. |
| Linear source drift | Inspect the source revision and changed task/branch/repository facts. Resolve the human/manual gate; do not overwrite the snapshot. |
| Repository/profile drift | Restore the exact frozen authority or deliberately terminate/recover through supported policy. Unrelated config edits must not retarget the run. |
| Codex interrupted | Restart with worker/`drive`; the controller inspects the started attempt and resumes only the persisted session when evidence permits. |
| Review findings loop | Inspect normalized finding source and persisted decisions. Every accepted clarification must be bound into fresh review; do not mark findings resolved in SQLite. |
| CI pending | Wait. The driver polls at the configured bound. |
| CI failed | A supported actionable failure becomes repair input; infrastructure/ambiguous failure may require attention. Inspect exact-head check evidence. |
| Stale approval | Ask the configured human reviewer to review and approve the current head after all code changes; old approval cannot be reused. |
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
| LaunchDaemon not running | Run LaunchDaemon `status`, verify the root-owned exact plist and worker-owned assets, then bootstrap or kickstart only when the sanitized next action permits it. |
| Supervisor conflict | Boot out and prove absence of the current service, preserve its plist under a non-`.plist` rollback name, and retry the selected supervisor preflight. Never load both. |
| Worker already running | Inspect both launchd domains and the process-lifetime lock owner. Do not remove the lock file or bypass the scheduler lease while a worker may still be alive. |

When evidence remains unclear, stop external writes and preserve sanitized
artifacts for review. The correct fallback is `manual_intervention`, not manual
state surgery.
