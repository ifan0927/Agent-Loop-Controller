#!/bin/sh
set -eu

repo_root="$(CDPATH= cd -- "$(dirname "$0")/.." && pwd -P)"
lab="$($repo_root/scripts/create-local-lab.sh)"
issue="$lab/simulated-issue.json"
decision="$lab/decision.json"

cat >"$issue" <<'JSON'
{
  "issue_id": "IFAN-LAB-RESUME-1",
  "title": "Choose and implement integer Clamp boundaries",
  "description": "Add mathutil.Clamp, but stop for a human decision before editing because the issue intentionally leaves boundary behavior undecided. Offer inclusive and exclusive boundary options.",
  "team": "IFAN",
  "labels": ["agent:codex", "fixture-owner/test-project"],
  "status": "Todo",
  "current_cycle": true,
  "cycle_id": "local-phase-1b",
  "repository_label": "fixture-owner/test-project",
  "base_branch": "main",
  "branch_name": "ifan/ifan-lab-resume-1-clamp",
  "goal": "Resolve boundary semantics, then implement Clamp with tests.",
  "acceptance_criteria": [
    "Ask whether min and max boundaries are inclusive or exclusive before editing.",
    "After the decision, implement Clamp and table-driven tests.",
    "go test ./... passes."
  ],
  "out_of_scope": ["Network access", "External services", "Git commits or pushes by Codex"],
  "verifier_ids": ["fixture-go-test"],
  "source_revision": "local-resume-lab-v1",
  "created_at": "2026-07-11T00:00:00Z",
  "updated_at": "2026-07-11T00:00:00Z",
  "comments": []
}
JSON

start_output="$(go run "$repo_root/cmd/ifan-loop" local start \
  --issue "$issue" \
  --registry "$lab/repository-registry.json" \
  --db "$lab/controller.db" --repository fixture-owner/test-project \
  --requester ifan0927 --requester-database-id 1 --requester-node-id MDQ6VXNlcjE= --requester-type User)"
printf '%s\n' "$start_output"
run_id="$(printf '%s\n' "$start_output" | sed -n 's/.*"run_id": "\([^"]*\)".*/\1/p' | head -n 1)"
outcome="$(find "$lab/runs/$run_id/attempts" -name implementation-outcome.json -type f | head -n 1)"
choice="$(sed -n 's/.*"recommendation": *"\([^"]*\)".*/\1/p' "$outcome" | head -n 1)"

cat >"$decision" <<JSON
{
  "choice_id": "$choice",
  "instructions": "Apply the selected boundary policy, then implement the function and table-driven tests."
}
JSON

go run "$repo_root/cmd/ifan-loop" local continue "$run_id" \
  --db "$lab/controller.db" \
  --registry "$lab/repository-registry.json" \
  --decision "$decision"

printf 'Local explicit-resume lab retained at %s\n' "$lab"
