# MVP Scope

## Objective

Prove one reliable vertical slice from an explicitly triggered, coding-ready
Linear issue to a human-approved squash-merged PR, using Mac Codex CLI as the
executor.

## In scope

- Manual `start IFAN-xxx` admission signal.
- Linear issue fetch, eligibility validation, and immutable snapshot.
- Repository mapping and Linear-provided branch name.
- Dedicated Git worktree per run.
- Durable SQLite run and transition state.
- Resumable Codex implementation session with structured output.
- Human decision checkpoint and explicit-session resume.
- Controller/repository-owned verifier registry and evidence capture. Linear may
  reference verifier IDs but never supply executable shell strings.
- Fresh independent Codex review of the complete branch delta.
- Repair, re-verification, and fresh re-review loop.
- PR creation only after internal review passes.
- CodeRabbit as the second automated reviewer.
- Human approval bound to the final PR head SHA.
- Squash merge and owned branch/worktree cleanup.
- Crash recovery and manual reconciliation command.

## Required safety properties

- One active run per Linear issue.
- No prompt or issue text is evaluated as a shell command.
- No `resume --last`; every resume names the persisted implementation session.
- No unsafe Codex sandbox bypass.
- No PR before a passing fresh review for the exact head.
- Any post-review code change invalidates prior automated and human approvals.
- No merge without green required checks and I-Fan's final approval.
- Controller deletes only resources it created and recorded as owned.

## Out of scope

- Cron polling and fully automatic admission.
- Linear webhook and custom Linear agent UI.
- Hermes-triggered execution and interactive routing.
- Codex Cloud as an executor.
- Automated deployment, production credentials, or destructive migrations.
- Multi-repository atomic work and one-issue-to-many-PR workflows.
- Automatic issue priority, cycle, or product-scope decisions.
- Autonomous Loop 4 changes to prompts, rules, skills, or controller policy.

## MVP completion criteria

1. A manual issue trigger produces one idempotent run.
2. The controller validates IFAN eligibility and freezes a task snapshot.
3. It provisions the exact Linear branch in a dedicated worktree.
4. It launches Codex, persists the session ID, and validates structured outcome.
5. It can pause for a human decision and resume the same session safely.
6. It independently verifies the candidate and records head-bound evidence.
7. A fresh Codex review passes before PR creation.
8. Findings from either Codex review or CodeRabbit enter the repair loop.
9. I-Fan approves the final head before squash merge.
10. Merge, Linear completion reconciliation, and owned cleanup succeed after a
    controller restart as well as during a normal run.
