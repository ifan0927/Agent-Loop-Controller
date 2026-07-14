# Isolated E2E Dogfood

## Purpose

This runbook is the acceptance evidence for issue #18. It exercises the
controller against one dedicated GitHub fixture repository and one coding-ready
Linear fixture issue. The controller repository and all STDS repositories are
never write targets.

The deterministic local fixture suite is a prerequisite, not a substitute for
this external run. It must use fake GitHub evidence and a disposable local bare
origin only.

## Operator-owned prerequisites

- One isolated GitHub fixture repository, with its numeric repository identity
  recorded in the inline `repositories` entry of the controller configuration.
- One clean local checkout whose `origin` is the configured fixture repository.
  The host's existing Git credential may be used; the controller validates the
  configured remote identity and never treats the credential as repository
  authority.
- One GitHub App installed only on the fixture repository. Its read permissions,
  optional PR-write permission, and optional squash-merge permission follow
  `docs/github-app-operator.md`.
- A protected, external GitHub App PEM file and an `IFAN_LOOP_LINEAR_TOKEN` in
  the operator environment. Neither belongs in this repository, SQLite,
  artifacts, command output, or this runbook.
- One Linear fixture issue satisfying the controller's IFAN, coding-ready,
  current-cycle, `agent:codex`, repository-label, branch-name, acceptance, and
  verifier policies.
- Branch protection on the fixture repository requiring current CI,
  stale-approval dismissal, and no bypass.
- Explicit operator authorization before launching the long-lived delivery
  driver, which may push, create a PR, squash merge only after I-Fan has
  reviewed, resolved the satisfied GitHub conversation, and approved the exact
  current head, and delete a controller-owned fixture branch.

Configure that repository with `origin_url` in the inline controller
configuration, for example
`git@github.com:fixture-owner/agent-loop-fixture.git`. `origin_path`
remains for the local bare fixture only. The checked-out `origin` may use the
equivalent HTTPS or SSH transport, but it must resolve to the exact same GitHub
owner and repository. URLs with embedded credentials, a non-GitHub host, or a
different repository are rejected before a fetch or write.

## Required local checks

Run the deterministic gate before a live attempt:

```sh
./scripts/verify-controller.sh
```

The gate runs formatting, normal and race Go tests, vet, the restart-safe GitHub
fixture replay, and a credential-like source scan. Test fixtures are excluded
because they deliberately exercise sanitization with fake credential-shaped
values. For a live attempt, repeat the content scan over each retained artifact
directory and the controller SQLite directory after the run:

```sh
./scripts/scan-sensitive-output.sh /absolute/path/to/run-artifacts /absolute/path/to/controller-state
```

Validate configuration before any network call:

```sh
go run ./cmd/ifan-loop config validate
go run ./cmd/ifan-loop config inspect
```

## Normal E2E procedure

The normal E2E execution uses one long-lived durable driver. It is not a
checklist of manually issued `push`, `open-pr`, `reconcile`, `merge`,
`reconcile-linear`, and `cleanup` commands. Start the configured fixture issue
once and retain the sanitized process output with the run evidence:

```sh
go run ./cmd/ifan-loop controller run IFAN-42 \
  --config /absolute/path/controller.json \
  --requester ifan0927 --requester-database-id '<id>' \
  --requester-node-id '<node-id>' --requester-type User
```

The driver writes the restart-safe run ID to sanitized stderr, derives each
legal action from freshly read persisted state, and remains alive while CI,
GitHub approval, or Linear completion is pending. It automatically
performs safe progression through
implementation, verification, fresh review, branch push, PR creation, review
reconciliation, guarded merge after the required human review sequence,
completion observation, and owned cleanup.

When the owned PR reaches `awaiting_human_approval`, I-Fan performs the human
review sequence in GitHub: review the repair, resolve each controller-replied
conversation that is satisfied, then approve the exact current head. No CLI
command may resolve or approve on I-Fan's behalf. The still-running driver only
observes the resolution and approval; if GitHub previously rejected a protected
merge, it re-reads the exact-head gates and makes at most one guarded retry.
Use `controller status` or `controller inspect` only to observe the sanitized
run and evidence during this wait.

If the controller process or host is intentionally restarted, resume the same
run with the long-lived driver:

```sh
go run ./cmd/ifan-loop controller drive '<persisted-run-id>' \
  --config /absolute/path/controller.json \
  --requester ifan0927 --requester-database-id '<id>' \
  --requester-node-id '<node-id>' --requester-type User
```

The default driver process has a 24-hour runtime limit; set `--max-runtime`
(at most seven days) for a deliberate longer fixture observation. A terminal
outcome, `awaiting_human_decision`, `manual_intervention`, an explicit process
signal, or expiry of that runtime ends this process. Resume an unfinished run
with `drive` after inspecting the persisted evidence. These are
operator-handling states, not automatic-repair permission. The lower-level
state commands are reserved for a documented recovery procedure or one of the
fault injections below; they are not required to complete the normal delivery
path.

A completed run can still expose a pending `operator_attention` record when
the configured source checkout was intentionally left untouched because it was
unsafe to synchronize. The current operator action is manual inspection and
synchronization of that checkout. This stable, sanitized read projection is for
future Hermes or UI consumers only: it has no notification transport,
acknowledgement, retry, or automatic remediation behavior.

For the specific case where a review repair creates a new verified candidate but
the existing owned PR branch update halted, an operator may use
`recover-owned-push` from `manual_intervention`. The command revalidates the
unchanged Linear task and the persisted open controller-owned PR, then restores
only the push gate. It does not write Git or GitHub state; the resumed driver
must still pass its normal exact-HEAD, remote, and fast-forward-lease checks.

## Acceptance matrix

| Case | Injection point | Expected result |
|---|---|---|
| Normal delivery | None | One long-lived driver creates one owned PR and reaches `completed` after I-Fan reviews, resolves satisfied controller-replied conversations, approves the exact head, and GitHub accepts the guarded squash merge. |
| Push restart | Push intent recorded, result unavailable | Restart the driver with `drive`; it observes the exact remote SHA or safely stops, with no force push or duplicate branch write. |
| PR restart | PR intent recorded, response unavailable | Restart the driver with `drive`; it adopts only the exact ownership marker/body digest PR and never creates a second PR. |
| Required-CI repair | A required CI check fails for the exact candidate head | The implementation session repairs the controller-generated check finding, then re-verifies, obtains a new fresh review, and fast-forwards the same owned PR branch to the new HEAD. |
| Owned PR push recovery | A repair update halted at `manual_intervention` | An explicit `recover-owned-push` may restore the push gate only after unchanged Linear and retained owned-PR proof; the driver revalidates before the eventual write. |
| Merge restart | Merge intent recorded, response unavailable | Restart the driver with `drive`; it re-reads GitHub and records the one authoritative squash merge, without a second merge write. |
| Linear pending | Merge observed, completion automation delayed | The running driver remains in `awaiting_linear_completion`; cleanup is prohibited. |
| Clean source synchronization | Completion observed after merge; configured source checkout is clean and its base HEAD equals the persisted squash merge SHA | The driver reaches `completed`, the source checkout remains at that exact merge SHA, and `operator_attention` is `[]`. |
| Dirty source synchronization | Completion observed after merge; configured source checkout has a dirty sentinel | The dirty source checkout and sentinel remain untouched; owned fixture resources are cleaned; the run reaches `completed` with one sanitized pending `source_checkout_sync_required` attention record. |
| Authority conflict | Remote, repository, installation, head, approval, or ownership mismatch | The run fails closed to documented operator intervention; it performs no speculative repair or write. |

## Deterministic review-lifecycle fixture matrix

This is an offline acceptance matrix for the trusted human feedback lifecycle.
`TestDurableReviewLifecycleFixtureDriver` uses the production coordinator and a
temporary real SQLite database with deterministic in-process fake Linear and
GitHub ports. It opens no network connection, credential source, subprocess,
or Git worktree. The fixture's fake repair controller records the normal
repair, verification, and fresh-review state edges without invoking Codex.
Its durable coverage is limited to the explicitly named driver scenarios below;
it does not claim to cover every restart boundary in this matrix.

| Fixture group | Required proof | Focused tests |
|---|---|---|
| Admission and repair | Only configured immutable `User` identities with an exact-head `CHANGES_REQUESTED` inline root become quoted repair input. Roots are sorted deterministically; lookalikes, Apps, Bots, replies, summaries, stale, resolved, outdated, and incomplete topology are inert. Identity/body/topology/head drift fails closed. | `TestNormalizeTrustedChangesRequested*`, `TestTrustedFeedbackDriftFailsClosedAfterAdmission` |
| Reply lifecycle | A verified repair records one intent before its fixed marker-bound root reply. Multiple roots are processed in stable order, one action at a time, with durable independent SQLite side effects across a new coordinator restart. A resolved root writes nothing. | `TestDurableReviewLifecycleFixtureDriver/multiple_roots_persist_independent_reply_intents_across_restart`, `TestProductionReplyReviewFeedbackResolvedAndInvalidEvidenceNeverPost` |
| Reply restart and ambiguity | Restart before POST, lost POST response, and persistence loss adopt exactly one matching configured-App marker. Wrong-App or multiple markers, inconclusive pagination, authority drift, and exhausted retries stop safely without a blind duplicate. | `TestProductionReplyReviewFeedbackRestartBeforePostAdoptsPersistedIntent`, `TestProductionReplyReviewFeedbackAdoptsRemoteSuccessAfterPersistenceInterruption`, `TestReplyAdoptionRejectsWrongAppAndMultipleMatches`, `TestProductionReplyReviewFeedbackInconclusiveReconciliationRequiresManualIntervention` |
| Merge protection wait | Actual adapter HTTP 405/409/422 rejections preserve their operation/status mapping. A durable 409 scenario enters `awaiting_github_mergeability`; a new coordinator observes it read-only, then a resolved thread with a newer observation timestamp permits one guarded retry. An unprotected scenario merges on its first request. SQLite preserves the immutable approval identity while recording its newer observation time. | `TestSquashMergePreservesPolicyEligibleHTTPStatus`, `TestDurableReviewLifecycleFixtureDriver/trusted_root_repair_reply_protected_merge_wait_restart_resolution_retry`, `TestDurableReviewLifecycleFixtureDriver/unprotected_merge_succeeds_on_first_authorized_request`, `TestApprovalAndMergeEvidenceAreImmutable` |
| Merge drift and recovery | Approval dismissal, a new exact-head change request, head drift, and base drift leave the wait state through normal approval, repair, or manual paths and never issue a second merge. | `TestDurableReviewLifecycleFixtureDriver/wait_rollback_*`, `TestProductionMergePolicyRetryFailsClosedWhenFollowupChangesTopology`, `TestProductionMergePolicyPendingPersistenceFailureCannotBypassOnRestart` |
| Reply error mapping | Actual reply endpoint fixtures classify 403/404 as authoritative no-retry rejection and preserve 429/network failures for reconciliation rather than treating them as success. | `TestReplyToReviewCommentMapsAuthoritativeAndRetryableFailures` |
| Sensitive-data boundary | Untrusted review bodies are prompt data only. Public reply text, side-effect intent/completion, inspect projections, idempotency markers, and request telemetry retain fixed prose or digests, never the body. | `TestProductionReplyReviewFeedbackKeepsUntrustedBodyOutOfPublicReplyAndIntent`, `TestInspectionProjectsTrustedFeedbackWithoutRawBody`, `TestInspectionNeverProjectsRawInlineFindingBody`, `TestReviewReplyMarkerAndFixedBodyAreDeterministic` |

Run this matrix without a live GitHub profile:

```sh
go test ./internal/adapters/sqlite -run 'Test(DurableReviewLifecycleFixtureDriver|ApprovalAndMergeEvidenceAreImmutable)' -count=1
go test ./internal/adapters/githubapp -run 'Test(ReplyToReviewCommentMapsAuthoritativeAndRetryableFailures|SquashMergePreservesPolicyEligibleHTTPStatus)' -count=1
```

The GitHub App adapter fixture suite also classifies bounded pagination, partial
GraphQL, rate limit, network-style unavailable responses, and 403/404 reply
rejection without exposing transport bodies. The production fixture tests bind
the named multi-root reply restart and protected-merge wait restart scenarios
to durable intent/evidence and assert no duplicate reply or merge side effect.

## Disposable source-sync acceptance

The source-synchronization matrix is a deterministic offline acceptance suite.
Each invocation creates its own temporary bare origin, source checkout,
controller worktree evidence, and SQLite database; it does not read or write a
configured operator checkout, GitHub, or Linear.

Run the clean exact-merge, restart-adoption, and no-attention case:

```sh
go test -race ./internal/adapters/sqlite -run 'TestDisposableSourceSyncFixtureMatrix/clean.*exact.*merge' -count=1
```

Run the dirty, wrong-branch, detached, and diverged source cases. They prove
the source HEAD and sentinel remain untouched while owned cleanup completes and
the sanitized attention projection is present:

```sh
go test -race ./internal/adapters/sqlite -run 'TestDisposableSourceSyncFixtureMatrix/unsafe.*source.*variants' -count=1
```

Run the complete offline matrix, including ahead/no-rewind and retryable
boundary cases:

```sh
go test -race ./internal/adapters/sqlite -run TestDisposableSourceSyncFixtureMatrix -count=1
```

## Evidence handoff

Retain the controller's sanitized inspection, transition history, verification
and review digests, GitHub/Linear observations, cleanup records, and the two
credential scans. Do not retain raw credentials, request authorization headers,
or private keys. A #18 pull request must summarize the completed matrix and use
`Fixes #18` in its description.
