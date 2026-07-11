# GitHub App Read-Only Operator Handoff

## Registration

- Suggested name: `I-Fan Agent Loop Controller Read Only` (append an environment suffix when needed).
- Disable callback URL and OAuth. The controller does not use user authorization.
- Disable webhooks for this phase.
- Grant read-only repository permissions for Metadata, Contents, Pull requests,
  Checks, Commit statuses, Actions, and Administration. Administration is read
  only and is needed to read required-check branch protection. Do not grant
  write permissions.
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

Create a configuration file outside the repository:

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
  "coderabbit_actor_id": 0,
  "coderabbit_node_id": "",
  "coderabbit_app_id": 0
}
```

Use zero/empty CodeRabbit identity until its numeric actor, node, and App
identity are confirmed from authoritative GitHub API evidence. This fails closed
for CodeRabbit trust rather than trusting a login or comment body.

Run the opt-in read-only smoke only against the selected fixture repository:

```sh
go run ./cmd/ifan-loop github-read \
  --config /absolute/protected/path/github-app.json \
  --db /absolute/path/controller.db \
  --run-id '<persisted-run-id>' \
  --pr 1 \
  --expected-head '<exact-head-sha>'
```

The next phase needs an isolated repository, one existing pull request with
known commits/checks/reviews, and this App installed only on that repository.
No write permission is required.
