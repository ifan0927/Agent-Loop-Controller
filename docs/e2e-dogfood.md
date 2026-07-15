# Second isolated live E2E

## Purpose and boundary

This is the canonical operator runbook for GitHub issue #42. It proves one
automatic admission from a dedicated IFAN Linear fixture issue through repair,
human review, guarded merge, Linear completion, exact source synchronization,
and owned cleanup.

The controller repository, all STDS repositories, and production repositories
are never write targets. The only delivery target is the configured LoopTest
fixture repository. Do not use `controller run IFAN-xxx` for this E2E: the
worker must admit the issue itself from Linear Todo.

## Preconditions

### Controller host

- `./scripts/verify-controller.sh` passes on the controller source to be built.
- A current-user-owned, non-symlink installed `ifan-loop` binary exists outside
  a repository checkout.
- The controller configuration is version 3, `0600`, validated, and has
  `automation.linear_todo_admission.enabled=true`.
- The admission authority pins the IFAN team, exact Todo and In Progress state
  IDs, one active run, the fixed trusted GitHub User requester, bounded polling,
  `local_outbox`, and `secret://file/linear-token`.
- The Linear token and GitHub App PEM are external protected files. Never print,
  copy, move, or retain either credential for this test.
- `config validate`, `config inspect`, `config doctor`, and LaunchAgent
  `doctor`/`validate` are healthy before the worker is loaded.

### Fixture repository and source checkout

- The configured profile names only `ifan0927/LoopTest`, its exact numeric
  repository and App-installation identities, `main`, and `fixture-go-test`.
- Its GitHub App is installed only on that fixture repository and has the
  minimal read, PR-create, review-reply, and guarded squash-merge permissions
  described in [the App handoff](github-app-operator.md).
- Branch protection requires the `fixture` check, one approving review, stale
  approval dismissal, conversation resolution, and no bypass.
- The configured local source checkout is clean, on `main`, and fast-forwarded
  to the configured base before the worker starts.

### Linear fixture issue

I-Fan creates one fresh coding-ready IFAN fixture issue and moves it to Todo.
It must be in the current cycle, have `agent:codex`, exactly one repository
label in the form `repo:<linear_label>` (for this fixture, `repo:looptest`).
The configured repository profile maps the short label to its GitHub owner and
repository identity. It must have the Linear-provided branch name, a bounded goal,
acceptance criteria, out-of-scope section, and the repository-owned verifier
ID. Do not create a second eligible Todo during the run. The controller must
not admit unrelated Todo work while this run is active.

## Preflight

Run only sanitized/read-only checks before loading the worker:

```sh
./scripts/verify-controller.sh
"$BIN" config validate --config "$CONFIG"
"$BIN" config inspect --config "$CONFIG"
"$BIN" config doctor --config "$CONFIG"
"$BIN" controller launchagent doctor --binary "$BIN" --config "$CONFIG"
"$BIN" controller launchagent validate --binary "$BIN" --config "$CONFIG" --plist "$PLIST"
```

Prepare and validate the plist as specified in
[the LaunchAgent runbook](launchagent-worker.md). The controller's install step
creates only an absent target plist and never overwrites an existing file.
Before installation, inspect the rendered file with `plutil -lint` and confirm
the target plist is absent.

## Execution

1. Run the separate bounded `launchagent bootstrap`, `status`, and
   `kickstart` steps for the exact validated label. The worker starts with no
   issue identifier; a timed-out step is operator attention and is not followed
   by an assumed-success restart.
2. Retain sanitized worker output. Observe one bounded Todo scan, one durable
   reservation, one Todo -> In Progress mutation, one run/worktree, and driver
   start.
3. Observe Codex implementation, verification, fresh independent review, one
   branch push, one owned PR, and the required `fixture` CI check.
4. I-Fan submits one exact-head `CHANGES_REQUESTED` inline root comment with a
   small verifiable code change. The controller must perform one bounded repair,
   invalidate old evidence, re-verify, fresh-review, update the same owned PR,
   and post exactly one fixed reply to that root.
5. While that thread is unresolved, intentionally restart only the worker or
   supervised process. Confirm the resumed run waits read-only and does not
   duplicate a repair, reply, or merge.
6. I-Fan reviews the repair, resolves the satisfied conversation, and submits
   an exact-head approval. The controller observes those GitHub facts and makes
   at most one guarded merge retry if protection previously rejected the merge.
7. Observe GitHub-authorized squash merge, Linear completion, source checkout
   synchronization to the persisted exact merge SHA, owned worktree/local
   branch/remote branch cleanup, and terminal `completed` state.

`controller status` and `controller inspect` are observation tools. Do not use
low-level state commands to advance this flow. A source-sync attention record
after completion means the source checkout was unsafe to touch; inspect and
synchronize it manually rather than modifying controller evidence.

## Stop, restore, and retain evidence

After completion, run the bounded `launchagent bootout` and `status` steps for
the test LaunchAgent, then restore the test-only
admission configuration as documented for the environment. Do not remove
controller state, artifacts, credentials, or logs merely to make the run look
clean.

Retain sanitized run/issue/PR identifiers, profile/task/evidence digests,
candidate and merge SHAs, state timeline, verifier/review/CI/approval bindings,
feedback and fixed-reply immutable IDs, resolution timestamps, merge/source-sync
and cleanup results, and worker restart evidence. Scan both the retained run
artifacts and controller-state directory:

```sh
./scripts/scan-sensitive-output.sh /absolute/path/to/run-artifacts /absolute/path/to/controller-state
```

Stop immediately and record a sanitized blocker on authority drift, an
unexpected candidate, duplicate run/branch/PR/reply/merge, incomplete reads,
wrong actor/App, missing protection, sensitive output, or unsafe source-sync
preconditions. Do not manually repair the fixture to force a passing result.

## Offline evidence that remains required

The live E2E relies on, but does not replace, deterministic coverage for:

- disposable source synchronization and ownership-safe cleanup;
- trusted feedback normalization, repair, reply idempotency, resolution waits,
  and protected-merge retries;
- one-active-run automatic admission, scheduler leases, and worker recovery.

`./scripts/verify-controller.sh` is the canonical entrypoint for that
deterministic gate.
