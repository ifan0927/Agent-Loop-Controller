# Agent Loop Controller

Agent Loop Controller is a deterministic, human-gated control plane for one
coding-ready Linear issue at a time. It owns workflow state and evidence;
Codex owns implementation and fresh review. Linear supplies the task,
GitHub supplies delivery and branch protection, and I-Fan remains the only
review, resolution, and approval authority.

## Current delivery model

The active acceptance path is local automatic Linear Todo admission:

```text
eligible Linear Todo
  -> worker reservation and Todo -> In Progress
  -> isolated worktree and Codex implementation
  -> controller verification and fresh independent review
  -> push, one owned PR, and required CI
  -> I-Fan change request / resolution / exact-head approval
  -> guarded squash merge, Linear completion, source sync, owned cleanup
```

The worker, driver, and SQLite journal are restart-safe. They may resume a
persisted run, but never create a second active run or repeat an external write
without reconciling durable evidence.

`controller run IFAN-xxx` and the low-level delivery commands remain bounded
recovery or local-lab interfaces. They are not the #42 live-E2E entrypoint.
That E2E starts a supervised worker with no issue identifier.

## Safety boundary

- Linear issue text is untrusted input, never a shell command.
- Each run freezes its task, repository profile, verifier policy, and exact
  evidence bindings.
- A code change invalidates the prior verification, fresh review, and approval.
- The controller only writes to its isolated fixture repository and resources it
  recorded as owned. It never targets this repository or an STDS repository.
- The controller never resolves GitHub conversations, approves reviews, bypasses
  branch protection, or uses personal `gh` credentials for delivery.
- Credentials stay outside the repository, SQLite projections, artifacts, logs,
  and documentation.

## Operator workflow

1. Prepare and validate the non-secret controller configuration and external
   credentials.
2. Verify the selected fixture repository, App permissions, branch protection,
   and clean source checkout.
3. Create one eligible IFAN fixture issue and move it to Todo.
4. Start the supervised worker without an issue identifier.
5. During the PR gate, I-Fan alone submits review feedback, resolves satisfied
   conversations, and approves the exact current head.
6. Retain sanitized evidence, scan retained state/artifacts, stop the worker,
   and restore the isolated test configuration.

The authoritative step-by-step procedure is [the isolated live E2E
runbook](docs/e2e-dogfood.md). Do not replace it with a sequence of manual
`push`, `open-pr`, `merge`, or cleanup commands.

## Verification

Run the controller gate before publishing a change:

```sh
./scripts/verify-controller.sh
```

It checks formatting, normal and race tests, vet, the GitHub read fixture, and
the sensitive-output scan. The deterministic fixture suites are necessary
preconditions for a live E2E, not a substitute for it.

## Document ownership

- [Architecture](docs/architecture.md): domain boundaries, durable state, and
  delivery invariants.
- [Configuration](docs/configuration.md): versioned local configuration,
  credentials boundary, and worker admission authority.
- [Isolated live E2E](docs/e2e-dogfood.md): the one canonical #42 operator
  procedure and evidence checklist.
- [GitHub App operator handoff](docs/github-app-operator.md): selected-repo
  permissions and GitHub protection requirements.
- [LaunchAgent worker runbook](docs/launchagent-worker.md): local macOS
  supervision lifecycle.
- [Controller/executor decision](docs/decisions/0001-controller-and-executor-boundary.md)
  and [Hermes handoff](docs/handoff/hermes.md): durable architectural context.

Historical issue-level scope and completed implementation slices live in GitHub
and Git history rather than duplicating current operating rules in this tree.
