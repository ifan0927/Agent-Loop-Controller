#!/bin/sh
set -eu

repo_root="$(CDPATH= cd -- "$(dirname "$0")/.." && pwd -P)"
cd "$repo_root"
go test ./internal/adapters/githubapp -run 'TestFixtureReplayAndRestartMint|Test401RefreshOnceAndRepeatedFailure|TestSecretSafeObservationsAndErrors' -count=1
