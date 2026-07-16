# GitHub App Security Runbook

This runbook owns the credential and repository-permission procedure for the
controller's GitHub App. General command use and delivery behavior belong in
[Operations](../operations.md); authority and evidence design belong in
[Architecture](../architecture.md).

## Safety Boundary

- Create a dedicated GitHub App; do not use a personal access token or `gh`
  authentication for controller delivery.
- Select **Only selected repositories**. Begin with the isolated fixture
  repository and never include production or STDS repositories in an E2E App.
- Disable OAuth/callbacks and webhooks for the current local controller.
- Keep the private key outside repositories, controller artifacts, SQLite,
  logs, shell history, and rendered plist files.
- Treat App ID, installation ID, repository database ID, and owner/name as one
  cross-checked authority set. Similar names are not sufficient.

## Permission Matrix

Grant only what the configured profile needs:

| GitHub repository permission | Level | Controller use |
| --- | --- | --- |
| Metadata | Read | Repository identity |
| Contents | Read | Repository/base evidence; change to write only for guarded merge |
| Checks | Read | Required check runs |
| Commit statuses | Read | Required status contexts |
| Actions | Read | Workflow/check evidence |
| Administration | Read | Required-check branch protection |
| Pull requests | Read | PR/review/thread evidence; change to write only for PR create/adopt |

The narrow configuration switches must match the App permission:

- `pull_requests_write=true` requires Pull requests **Read and write** and
  enables only create/adopt of one marker-owned PR.
- `review_comments_write=true` requires Pull requests **Read and write** and
  enables only one marker-bound reply to an already admitted inline root
  comment.
- `squash_merge_write=true` requires Contents **Read and write** and enables only
  an exact-head conditional squash merge.

These switches do not authorize review submission, approval, conversation
resolution, close, arbitrary comment, branch deletion, settings changes, merge
commit/rebase, auto-merge, force push, or administrator bypass.

## Register and Install

1. Register the App under the intended owner with OAuth, callback URL, and
   webhooks disabled.
2. Apply the minimum read permissions above. Add write permissions only for the
   isolated repository and tested capability.
3. Install the App on exactly the selected repository.
4. Record the numeric App ID, installation ID, repository database ID, and
   exact owner/name in the private controller configuration.
5. Confirm branch protection independently: required checks, at least one human
   approval, stale-approval dismissal, conversation resolution, up-to-date
   branch policy as required, and no App/admin bypass.

GitHub's merge endpoint conditions the write on PR head, not an expected base
SHA. Branch protection is therefore the final base-freshness authority. The
controller records and re-reads base evidence but does not rebase or bypass a
rejected merge.

## Generate and Store the Private Key

Generate a key in GitHub App settings and place the PEM at an absolute canonical
path outside this repository. The leaf must be a current-user-controlled,
non-symlink regular file with no group/world access. Avoid cloud-synced or shared
directories.

Never display the PEM with `cat`, paste it into chat, encode it in configuration,
put it in an environment printed by diagnostics, or copy it into artifacts.

Configuration contains only the external reference:

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
  "squash_merge_write": false,
  "review_comments_write": false
}
```

The values above are placeholders, not usable identities.

## Validate Without Exposing Credentials

```sh
ifan-loop config validate --config /absolute/private/controller.json
ifan-loop config inspect --config /absolute/private/controller.json
ifan-loop config doctor --config /absolute/private/controller.json
```

`config validate` and `config inspect` validate key-path topology but do not read
the key. A production GitHub command opens it only after requester/run/profile
authorization. The client mints short-lived installation tokens in memory and
persists only sanitized installation/request metadata and response digests.

Use `github-read` only as an opt-in diagnostic for an already persisted isolated
run, with its exact requester, repository, state, idempotency, PR, and head
authority. It is not a setup probe and must never target an arbitrary PR.

## Enable a Write Capability

1. Stop the worker.
2. Confirm the App is installed only on the intended repository.
3. Change only the necessary App permission.
4. Change only the corresponding profile switch.
5. Run offline configuration checks and a deterministic fixture gate.
6. Recheck repository branch protection and no-bypass settings.
7. Exercise the capability only in the isolated E2E or a specifically approved
   production run.
8. Inspect persisted intent/observation and scan retained output for secrets.

Do not enable all switches as a convenience. A read-only profile should keep
all three false.

## Rotate or Revoke the Key

1. Stop the worker or wait until no GitHub operation is in flight.
2. Generate a new App key.
3. Write it as a new protected regular file; do not overwrite through a symlink
   or expose bytes in terminal output.
4. Atomically change the private configuration reference.
5. Re-run validation and one authorized read against the selected repository.
6. Revoke the old GitHub key only after the new key succeeds.
7. Securely handle the old external file under the operator's credential policy;
   never commit it or retain it as run evidence.

Changing credential bytes or the external path does not retarget an active run
when the frozen App/installation/repository identities are unchanged.

## Incident Response

On suspected key/token exposure:

1. Stop the worker and external writes.
2. Revoke the affected App key in GitHub; installation tokens are short-lived
   but should be treated as compromised until expiry.
3. Review selected repositories and App permission changes.
4. Inspect controller GitHub request observations and repository audit evidence
   without copying sensitive payloads.
5. Rotate to a new protected key and validate read-only access first.
6. Record only sanitized incident facts in issues. Never paste the credential,
   authorization header, raw token response, or private artifact.

If repository selection or App identity cannot be proven, leave the run stopped
in manual intervention. Do not broaden permissions to diagnose an authority
failure.
