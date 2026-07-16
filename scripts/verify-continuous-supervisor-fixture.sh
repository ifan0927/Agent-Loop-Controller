#!/bin/sh
set -eu

repo_root="$(CDPATH= cd -- "$(dirname "$0")/.." && pwd -P)"
summary="$repo_root/testdata/continuous-supervisor-fixture-summary.json"
actual="$(mktemp "${TMPDIR:-/tmp}/ifan-fixture-summary.XXXXXX")"
events="$(mktemp "${TMPDIR:-/tmp}/ifan-fixture-events.XXXXXX")"
trap 'rm -f "$actual" "$events"' EXIT HUP INT TERM

cd "$repo_root"
{
  go test -json ./cmd/ifan-loop -count=1 -run '^(TestAdmissionWorkerHasNoSevenDayProcessExpiry|TestAdmissionWorkerHasNoSevenDayExpiryWhileDriverPolls|TestAdmissionWorkerCancellationDuringOnceDispatchIsAStatus|TestControllerWorkerSubprocessSIGTERMClosesCompleteRuntime|TestOfflineAcceptanceWorkerRestartPreservesRetryAndParksAtDurableAttention|TestOfflineParkedDecisionSurvivesRestartAndAutomaticallyReturnsToDriver|TestOfflineSQLiteAdmissionSelectsTotalOrderFromThreeCandidates)$'
  go test -json ./internal/adapters/sqlite -count=1 -run '^TestAutomaticAdmissionAbandonReleasesSlotAndReplaysIdempotently$'
  go test -json ./internal/application -count=1 -run '^TestOfflineAcceptanceProductionAbandon(CompletesOwnedCleanup|TerminalizesWithResidueAndReplaysCleanup)$'
} | tee "$events" | go run ./cmd/fixture-summary --expected "$summary" >"$actual"

"$repo_root/scripts/scan-sensitive-output.sh" "$actual" "$events"
