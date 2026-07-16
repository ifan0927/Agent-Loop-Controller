# Isolated Live E2E Runbook

This is the high-risk, destructive acceptance procedure for the current second
live E2E. It may create and delete controller-owned fixture branches, create and
merge one fixture PR, and mutate one dedicated Linear issue from Todo to In
Progress. It must never target this repository, an STDS repository, or a
production repository.

The active tracker is
[GitHub issue #42](https://github.com/ifan0927/Agent-Loop-Controller/issues/42).

## Required Boundary

- Target only the configured isolated LoopTest fixture repository and dedicated
  GitHub App installation.
- Use one fresh coding-ready IFAN fixture issue.
- Start with `controller worker`; do not use `controller run IFAN-xxx` or
  low-level recovery commands to advance the normal flow.
- I-Fan alone creates/moves the issue, submits trusted feedback, resolves the
  conversation, and gives final exact-head approval.
- Retain sanitized evidence; never retain credentials, headers, private key
  material, raw token responses, or unnecessary private paths.

## Preconditions

### Controller and configuration

- `./scripts/verify-controller.sh` and `git diff --check` pass on the exact
  controller source used to build the installed binary.
- A current-user-owned, non-symlink installed binary exists outside a repository
  checkout.
- Version 3 mode-`0600` configuration validates and enables automatic admission
  with the exact IFAN team, Todo/In Progress state IDs, fixed trusted requester,
  bounded scan/poll/lease limits, one active run, `local_outbox`, and file
  credential source.
- `config validate`, `config inspect`, `config doctor`, LaunchAgent `doctor`,
  `validate`, and installed plist validation are healthy.
- Linear token, GitHub App PEM, database directory, logs, run roots, and
  worktree roots meet private path/permission requirements.

### Fixture repository

- The profile names only the exact LoopTest owner/name, numeric repository,
  installation/App identities, base branch, and fixture verifier.
- The App has only the capabilities in the
  [GitHub App runbook](github-app.md).
- Branch protection requires the fixture check, human approval, stale approval
  dismissal, conversation resolution, and no bypass.
- The configured source checkout is clean, on the configured base branch, and
  fast-forwarded to the current remote base.

### Linear fixture issue

I-Fan creates one fresh issue in team IFAN and current cycle with:

- state Todo;
- label `agent:codex` and no `agent:hermes`;
- exactly one configured `repo:<linear_label>` label;
- a valid Linear-provided branch name;
- bounded `## Goal` or `## Outcome`, `## Acceptance Criteria`, and optional
  `## Out of Scope` content.

Do not create another eligible top-priority Todo during the run. The verifier
comes from repository configuration, never issue text.

## Preflight

Run sanitized checks without reading credential contents into output:

```sh
./scripts/verify-controller.sh
git diff --check
"$BIN" config validate --config "$CONFIG"
"$BIN" config inspect --config "$CONFIG"
"$BIN" config doctor --config "$CONFIG"
"$BIN" controller launchagent doctor --binary "$BIN" --config "$CONFIG" --plist "$PLIST"
"$BIN" controller launchagent validate --binary "$BIN" --config "$CONFIG" --plist "$PLIST"
"$BIN" controller launchagent plist-validate --binary "$BIN" --config "$CONFIG" --plist "$PLIST"
```

If installing a new plist, render to a private temporary file, run
`plutil -lint`, prove the target absent, and use the absent-only install command.
Do not overwrite an existing plist.

## Execution

1. Bootstrap and observe the exact LaunchAgent. Kickstart only when its bounded
   status result recommends it. A timeout is operator attention, not success.
2. Observe the worker start with no issue identifier, acquire its lease, scan a
   bounded queue, select the unique priority, reserve the issue, persist
   mutation intent, move only that issue to In Progress, and create one run.
3. Observe one isolated worktree, Codex implementation, exact-head verification,
   fresh independent review, branch push, one owned PR, and passing required CI.
4. I-Fan submits one exact-current-head inline root `CHANGES_REQUESTED` review
   with a small verifiable code change.
5. Observe one trusted feedback record, bounded same-session repair, new
   candidate head, invalidation of old evidence, passing verification/fresh
   review/CI, and exactly one marker-bound controller reply to the root comment.
6. While the conversation remains unresolved, restart only the worker/supervised
   process. Confirm restart resumes a read-only wait without duplicate repair,
   reply, push, PR, or merge.
7. I-Fan reviews the repair, resolves the satisfied conversation, and approves
   the exact repaired head.
8. Observe protected squash merge, Linear completion observation, source
   checkout fast-forward to the persisted merge SHA, owned worktree/local/remote
   branch cleanup, and terminal `completed`.
9. Confirm no unrelated Todo was admitted while the run was nonterminal.

Use `controller status` and `controller inspect` for observation. Do not use
manual `continue`, `push`, `open-pr`, `reconcile`, `merge`, `reconcile-linear`,
or `cleanup` to manufacture acceptance.

## Restart Checkpoint

Before the controlled restart, retain sanitized evidence showing:

- current state and run/profile/task/idempotency digests;
- original and repaired candidate heads;
- trusted root/review/thread identity and reply lifecycle;
- verified reply evidence or pending reply intent;
- unresolved conversation and absence of merge intent/result.

After restart, prove the same run and identities resume, attempt/reply/side-
effect counts do not duplicate, and the driver remains read-only until I-Fan's
external actions change GitHub authority.

## Evidence Checklist

Retain sanitized:

- run, issue, PR, profile, task, and configuration digests/identities;
- state timeline and worker lease/restart/queue-decision observations;
- base, original candidate, repaired candidate, and merge SHAs;
- verifier, fresh-review, required-CI, and approval exact-head bindings;
- trusted feedback/reply immutable IDs, marker digest, and resolution time;
- Linear mutation, push, PR, reply, merge, completion, sync, and cleanup
  intent/observation status;
- source checkout before/after and owned resource results.

Do not retain raw credential material or copy unbounded raw comment/API content
outside the controller's private evidence store.

## Stop Conditions

Stop immediately and preserve sanitized evidence on:

- wrong or unexpected candidate, repository, App, installation, actor, issue,
  branch, PR, or Git SHA;
- duplicate run, mutation, branch write, PR, repair, reply, merge, or cleanup;
- incomplete/paginated/partial external reads or authority drift;
- missing required protection or bypass-capable App/user;
- unexpected manual intervention, internal classification error, or retry
  exhaustion;
- sensitive output;
- dirty/diverged source checkout or cleanup ownership conflict.

Do not edit SQLite, delete artifacts, manually alter fixture code/branches, or
invoke recovery commands to force a pass. Open a bounded defect or stabilization
issue with sanitized evidence.

## Completion, Shutdown, and Restore

After `completed`:

1. Run the bounded LaunchAgent `bootout` and confirm `status` reports it absent.
2. Restore the test-only automatic-admission configuration under the operator's
   environment procedure.
3. Confirm the fixture source is exactly the persisted merge commit and all
   expected controller-owned worktree/local/remote branch resources are gone.
4. Keep SQLite, artifacts, audit evidence, and private logs long enough for
   review; do not erase evidence to make the run appear clean.
5. Scan both artifact and controller state roots:

```sh
./scripts/scan-sensitive-output.sh /absolute/run-artifacts /absolute/controller-state
```

6. Record the pass/failure outcome in issue #42 without credentials or private
   evidence.

The live pass supplements rather than replaces deterministic restart, source
sync, trusted-feedback, admission, and retry fixtures in
`./scripts/verify-controller.sh`.
