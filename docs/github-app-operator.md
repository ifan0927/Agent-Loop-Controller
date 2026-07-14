# GitHub App Operator Handoff

## Registration

- Suggested name: `I-Fan Agent Loop Controller Read Only` (append an environment suffix when needed).
- Disable callback URL and OAuth. The controller does not use user authorization.
- Disable webhooks for this phase.
- Grant read-only repository permissions for Metadata, Contents, Checks, Commit
  statuses, Actions, and Administration. Administration is read only and is
  needed to read required-check branch protection.
- Keep Pull requests read-only by default. A controller profile may set
  `pull_requests_write` to `true` only for the isolated fixture repository and
  only after the App's Pull requests permission is changed to **Read and write**.
  This enables exactly one controller operation: create-or-adopt a PR with a
  persisted ownership marker. It does not authorize comments, reviews, merge,
  close, branch deletion, or any generic GitHub write client.
- A merge-capable isolated fixture profile must additionally set
  `squash_merge_write` to `true` and change only the App's **Contents**
  repository permission to **Read and write**. GitHub requires Contents write
  for the conditional `PUT /pulls/{number}/merge` endpoint. This capability is
  limited to a persisted exact-head squash request; it does not enable merge
  commits, rebase, auto-merge, force, admin bypass, reviews, comments, branch
  deletion, or repository settings. Keep both write capabilities disabled for
  ordinary read-only profiles.
- Select **Only selected repositories** and initially choose only an isolated
  fixture repository. Do not select production or STDS repositories.

Record the numeric App ID from the App settings page. After installation, record
the numeric installation ID from the installation URL or the GitHub App REST
response. Record the fixture repository numeric database ID and owner/name.

Generate a private key in the GitHub App settings. Store the PEM outside this
repository in a protected regular file. Do not use a symlink, copy it into run
artifacts, or commit it. Rotate by generating a new key, atomically changing the
external file reference, validating read-only access, and then revoking the old
key. Installation tokens are minted at runtime and must never be stored.

## Local configuration

Add each profile to the `github_app_profiles` array in the single controller
configuration outside the repository. The default macOS file is
`~/Library/Application Support/agent-loop-controller/controller.json`; use
`ifan-loop config init` to create its secret-free skeleton. The private key
remains a separate protected file.

```json
{
  "api_base_url": "https://api.github.com",
  "graphql_url": "https://api.github.com/graphql",
  "app_id": 123,
  "installation_id": 456,
  "repository_owner": "example-owner",
  "repository_name": "isolated-fixture",
  "repository_id": 789,
  "private_key_file": "/absolute/protected/path/app.pem",
  "http_timeout": "15s",
  "token_refresh_skew": "5m",
  "api_version": "2022-11-28",
  "pull_requests_write": false,
  "squash_merge_write": false
}
```

Run the opt-in read-only smoke only against the selected fixture repository:

```sh
go run ./cmd/ifan-loop github-read \
  --config /absolute/protected/path/controller.json \
  --run-id '<persisted-run-id>' \
  --requester ifan0927 --requester-database-id '<id>' \
  --requester-node-id '<node-id>' --requester-type User \
  --repository owner/isolated-fixture \
  --expected-state '<persisted-state>' \
  --idempotency-key '<persisted-key>' \
  --pr 1 \
  --expected-head '<exact-head-sha>'
```

`controller run` starts the normal long-lived delivery driver, and `controller
drive` resumes it after a restart. They derive and perform the legal GitHub
operations from the persisted run; an operator does not normally copy a state
and idempotency key between `push`, `open-pr`, `reconcile`, and `merge` commands.
Requester-authorized `controller status` and `controller inspect` may expose the
run-scoped idempotency key for an audited recovery procedure. It is not a GitHub
App private key, installation token, or Linear credential; requester
authorization and the expected state remain mandatory for every low-level
recovery operation.

`controller recover-owned-push` is reserved for a halted repair fast-forward on
an existing controller-owned open PR. It accepts only `manual_intervention` and
proves stable Linear source plus persisted PR ownership before returning the run
to the guarded push gate. It has no GitHub App write and no Git write of its
own; the resumed driver performs the normal exact-HEAD and lease checks.

The driver polls pending GitHub and Linear evidence every 30 seconds by default
and exits after 24 hours. An operator may deliberately set `--poll-interval`,
`--max-immediate-actions`, or `--max-runtime` (no more than seven days) for an
isolated E2E observation. A stopped or runtime-limited process is resumed with
`controller drive`; it does not broaden the controller's write authority.

The following `open-pr` command is a recovery/debug adapter exercise only. Do
not use it as a normal delivery step: after the exact candidate branch is ready,
the driver opens or adopts the single controller-owned PR itself. To deliberately
exercise recovery against an isolated fixture, change only the fixture profile
and App permission as described above, then invoke:

```sh
go run ./cmd/ifan-loop controller open-pr '<persisted-run-id>' \
  --config /absolute/protected/path/controller.json \
  --requester ifan0927 --requester-database-id '<id>' \
  --requester-node-id '<node-id>' --requester-type User \
  --repository owner/isolated-fixture \
  --expected-state branch_pushed --idempotency-key '<persisted-key>'
```

The adapter first validates the installation token and numeric repository
identity, then looks up an exact marker/body-digest match before POSTing. A
transport interruption leaves immutable intent in SQLite; a later invocation
adopts only that exact PR and never adopts a merely matching branch or title.

To exercise the guarded merge path, use a separate selected-repository
installation with the two write switches described above. During normal
operation, the driver re-reads the PR, exact head/base, required checks, fresh
local Sol evidence, and I-Fan's immutable-identity approval immediately before
recording the merge intent. It sends only `merge_method:
squash` with the expected head SHA, then re-reads the PR to persist its merge SHA
and timestamp. If the process loses a successful response, `controller drive`
observes the merged PR and does not send another merge request; a closed-but-
unmerged PR fails closed.

## Base freshness boundary

GitHub's direct merge endpoint conditions the write on the PR head SHA, not an
expected base SHA. The controller therefore records and re-reads the current
base immediately before the conditional squash request, but does not rebase or
retry a rejected merge. The selected repository must make GitHub branch
protection the final base-freshness authority: require branches to be up to
date, require the configured CI, dismiss stale approvals after new commits,
and do not allow bypass. A GitHub rejection or conflicting evidence is persisted as
`manual_intervention` for an operator to resolve; it is not a controller repair
action.
