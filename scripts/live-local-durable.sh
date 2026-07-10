#!/bin/sh
set -eu

repo_root="$(CDPATH= cd -- "$(dirname "$0")/.." && pwd -P)"
lab="$($repo_root/scripts/create-local-lab.sh)"

go run "$repo_root/cmd/ifan-loop" local start \
  --issue "$lab/simulated-issue.json" \
  --registry "$lab/repository-registry.json" \
  --db "$lab/controller.db" \
  --run-root "$lab/runs" \
  --worktree-root "$lab/worktrees"

printf 'Local durable lab retained at %s\n' "$lab"
