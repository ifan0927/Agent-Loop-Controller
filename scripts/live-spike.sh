#!/bin/sh
set -eu

root="$(mktemp -d "${TMPDIR:-/tmp}/ifan-loop-live.XXXXXX")"
remote="$root/origin.git"
workspace="$root/workspace"
artifacts="$root/artifacts"
task="$root/task.json"

git init --bare "$remote"
git init -b main "$workspace"
git -C "$workspace" config user.name "Agent Loop Fixture"
git -C "$workspace" config user.email "fixture@example.invalid"
mkdir -p "$workspace/mathutil" "$artifacts"
printf '%s\n' 'module example.invalid/fixture' '' 'go 1.26' >"$workspace/go.mod"
printf '%s\n' 'package mathutil' >"$workspace/mathutil/doc.go"
git -C "$workspace" add --all
git -C "$workspace" commit -m "Fixture base"
git -C "$workspace" remote add origin "$remote"
git -C "$workspace" push origin main
git -C "$workspace" switch -c fixture/phase-1a

cat >"$task" <<'JSON'
{
  "run_id": "live-spike-001",
  "issue_id": "FIXTURE-1",
  "issue_url": "local://fixture/FIXTURE-1",
  "title": "Add deterministic integer addition",
  "description": "In package mathutil, add a pure Add(a, b int) int function and table-driven unit tests.",
  "repository": "local/disposable-fixture",
  "base_branch": "main",
  "working_branch": "fixture/phase-1a",
  "goal": "Add the small pure function and tests without changing module configuration.",
  "acceptance_criteria": [
    "mathutil.Add returns the sum of two integers.",
    "Table-driven unit tests cover positive, negative, and zero inputs.",
    "go test ./... passes."
  ],
  "out_of_scope": ["Network access", "External services", "Git commits or pushes by Codex"],
  "verifier_ids": ["fixture-go-test"],
  "policy": {
    "human_approval_required": true,
    "merge_method": "squash",
    "max_repair_attempts": 0,
    "allow_scope_expansion": false,
    "create_derived_issues": false
  },
  "source_revision": "fixture-v1",
  "created_at": "2026-07-11T00:00:00Z"
}
JSON

go run ./cmd/ifan-loop spike --task "$task" --workspace "$workspace" --artifacts "$artifacts"
printf 'Live fixture retained at %s\n' "$root"
