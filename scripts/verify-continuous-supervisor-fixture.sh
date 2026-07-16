#!/bin/sh
set -eu

repo_root="$(CDPATH= cd -- "$(dirname "$0")/.." && pwd -P)"
summary="$repo_root/testdata/continuous-supervisor-fixture-summary.json"
actual="$(mktemp "${TMPDIR:-/tmp}/ifan-fixture-summary.XXXXXX")"
trap 'rm -f "$actual"' EXIT HUP INT TERM

cd "$repo_root"
{
  go test -json ./cmd/ifan-loop -count=1 -run '^(TestAdmissionWorkerHasNoSevenDayExpiryWhileDriverPolls|TestControllerWorkerSubprocessSIGTERMClosesCompleteRuntime|TestOfflineAcceptanceWorkerRestartPreservesRetryAndParksAtDurableAttention|TestOfflineParkedDecisionSurvivesRestartAndAutomaticallyReturnsToDriver|TestOfflineSQLiteAdmissionSelectsTotalOrderFromThreeCandidates)$'
  go test -json ./internal/adapters/sqlite -count=1 -run '^TestAutomaticAdmissionAbandonReleasesSlotAndReplaysIdempotently$'
  go test -json ./internal/application -count=1 -run '^TestOfflineAcceptanceProductionAbandonTerminalizesWithResidueAndReplaysCleanup$'
} | go run ./cmd/fixture-summary --expected "$summary" >"$actual"

"$repo_root/scripts/scan-sensitive-output.sh" "$actual"
