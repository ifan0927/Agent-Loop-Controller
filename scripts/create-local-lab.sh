#!/bin/sh
set -eu

if [ "$#" -gt 1 ]; then
  echo "usage: $0 [lab-directory]" >&2
  exit 2
fi

if [ "$#" -eq 1 ]; then
  root="$1"
  mkdir -p "$root"
  if [ -n "$(find "$root" -mindepth 1 -maxdepth 1 -print -quit)" ]; then
    echo "lab directory must be empty: $root" >&2
    exit 1
  fi
else
  root="$(mktemp -d "${TMPDIR:-/tmp}/Agent-Loop-Controller-lab.XXXXXX")"
fi

root="$(cd "$root" && pwd -P)"
origin="$root/origin.git"
source="$root/source"
worktrees="$root/worktrees"
runs="$root/runs"
issue="$root/simulated-issue.json"
registry="$root/repository-registry.json"

git init --bare "$origin" >/dev/null
git init -b main "$source" >/dev/null
git -C "$source" config user.name "Agent Loop Fixture"
git -C "$source" config user.email "fixture@example.invalid"
mkdir -p "$source/mathutil" "$worktrees" "$runs"
printf '%s\n' 'module example.invalid/local-durable-lab' '' 'go 1.26' >"$source/go.mod"
printf '%s\n' 'package mathutil' >"$source/mathutil/doc.go"
printf '%s\n' 'ignored.tmp' >"$source/.gitignore"
git -C "$source" add --all
git -C "$source" commit -m "Fixture base" >/dev/null
git -C "$source" remote add origin "$origin"
git -C "$source" push origin main >/dev/null 2>&1

cat >"$registry" <<JSON
{
	"version": 1,
  "repositories": [
    {
	  "owner": "fixture-owner",
	  "name": "test-project",
      "origin_path": "$origin",
      "source_path": "$source",
	  "run_root": "$runs",
	  "worktree_root": "$worktrees",
      "base_branch": "main",
	  "verifier_registry_ref": "builtin:v1",
	  "verifier_ids": ["fixture-go-test"],
	  "github_app_profile_ref": "github-app-profile:fixture-readonly",
	  "github_installation_id": 1,
	  "expected_repository_id": 1,
	  "operator_identity_policy": {"allowed_logins": ["ifan0927"]}
    }
  ]
}
JSON

cat >"$issue" <<'JSON'
{
  "issue_id": "IFAN-LAB-1",
  "title": "Add deterministic integer Clamp",
  "description": "In package mathutil, add a pure Clamp(value, min, max int) int function and table-driven unit tests. Assume min is less than or equal to max.",
  "team": "IFAN",
  "labels": ["agent:codex", "fixture-owner/test-project"],
  "status": "Todo",
  "current_cycle": true,
  "cycle_id": "local-phase-1b",
  "repository_label": "fixture-owner/test-project",
  "base_branch": "main",
  "branch_name": "ifan/ifan-lab-1-clamp",
  "goal": "Add a small deterministic Clamp function with tests.",
  "acceptance_criteria": [
    "Clamp returns min when value is below min.",
    "Clamp returns max when value is above max.",
    "Clamp returns value when it is inside the inclusive range.",
    "Table-driven tests cover below, inside, and above the range.",
    "go test ./... passes."
  ],
  "out_of_scope": ["Network access", "External services", "Git commits or pushes by Codex"],
  "verifier_ids": ["fixture-go-test"],
  "source_revision": "local-lab-v1",
  "created_at": "2026-07-11T00:00:00Z",
  "updated_at": "2026-07-11T00:00:00Z",
  "comments": ["Keep module configuration unchanged."]
}
JSON

printf '%s\n' "$root"
