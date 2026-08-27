#!/bin/sh
set -eu

repo_root="$(CDPATH= cd -- "$(dirname "$0")/.." && pwd -P)"
cd "$repo_root"

if ! command -v rg >/dev/null 2>&1; then
  printf '%s\n' 'controller_verification:missing_dependency:ripgrep' >&2
  exit 2
fi

format_output="$(gofmt -d cmd internal)"
if [ -n "$format_output" ]; then
  printf '%s\n' 'Go source is not gofmt-formatted:' >&2
  printf '%s\n' "$format_output" >&2
  exit 1
fi

go test ./...
go test -race -p=1 -timeout=15m ./...
go vet ./...
./scripts/live-github-read-fixture.sh
./scripts/verify-continuous-supervisor-fixture.sh
./scripts/test-scan-sensitive-output.sh
./scripts/scan-sensitive-output.sh .
