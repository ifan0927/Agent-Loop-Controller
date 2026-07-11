#!/bin/sh
set -eu

repo_root="$(CDPATH= cd -- "$(dirname "$0")/.." && pwd -P)"
lab="$($repo_root/scripts/create-local-lab.sh)"

start_output="$(go run "$repo_root/cmd/ifan-loop" local start \
  --issue "$lab/simulated-issue.json" \
  --registry "$lab/repository-registry.json" \
  --db "$lab/controller.db")"
printf '%s\n' "$start_output"
run_id="$(printf '%s\n' "$start_output" | sed -n 's/.*"run_id": "\([^"]*\)".*/\1/p' | head -n 1)"
candidate="$(printf '%s\n' "$start_output" | sed -n 's/.*"candidate_head": "\([^"]*\)".*/\1/p' | head -n 1)"
approval="$lab/explicit-human-approval.json"

printf '{"pr_number":1,"approver":"ifan0927","source":"fixture_explicit_approval","approved_sha":"%s","ci_status":"pass","coderabbit_status":"pass","internal_review_sha":"%s","approved_at":"2026-07-11T00:00:00Z"}\n' "$candidate" "$candidate" >"$approval"

# Starting a second public CLI process is the required restart boundary.
go run "$repo_root/cmd/ifan-loop" local status "$run_id" --db "$lab/controller.db"
go run "$repo_root/cmd/ifan-loop" local fixture-deliver "$run_id" --db "$lab/controller.db" --registry "$lab/repository-registry.json" --approval "$approval"
go run "$repo_root/cmd/ifan-loop" local inspect "$run_id" --db "$lab/controller.db"

test ! -e "$lab/worktrees/$run_id"
test -z "$(git -C "$lab/source" ls-remote origin "refs/heads/ifan/ifan-lab-1-clamp")"
test -n "$(git -C "$lab/source" ls-remote origin refs/heads/main)"

printf 'Post-approval dogfood lab retained at %s\n' "$lab"
