#!/bin/sh
set -eu

repo_root="$(CDPATH= cd -- "$(dirname "$0")/.." && pwd -P)"
cd "$repo_root"
go test ./internal/adapters/githubapp -run 'TestVersionedFixtureScenarioIndex|TestFixtureReplayAndRestartMint|Test401RefreshOnceAndRepeatedFailure|TestSecretSafeObservationsAndErrors|TestHTTPFailureClassificationAndBounds|TestLatestCheckRunWinsDeterministically' -count=1
go test ./cmd/ifan-loop -run TestGitHubReadCLIEndToEndPersistsAndRestarts -count=1
