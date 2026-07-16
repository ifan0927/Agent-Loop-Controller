# AGENTS.md

All user-facing discussion is in Traditional Chinese. All code comments and
committed technical documentation are in English unless a document explicitly
targets I-Fan or Hermes in Traditional Chinese.

## Mission

Build a deterministic, human-gated controller that translates a coding-ready
Linear issue into an isolated Codex Exec delivery loop. The controller owns
workflow state and evidence. Codex owns code reasoning and implementation.

## MVP boundaries

- Linear is the task source of truth.
- The controller is a deterministic state machine, not an LLM agent.
- Every run uses a dedicated worktree and the Linear-provided branch name.
- Implementation uses a resumable `codex exec` session.
- A fresh, independent Codex Exec review run must pass before a PR is opened.
- I-Fan is the final approval gate. Agents never approve their own work.
- Any code change after a review invalidates that review and requires a new one.
- Controller policy, Git state, tests, CI, and GitHub state are authoritative;
  an agent's natural-language claim is not evidence by itself.

## Out of scope for the MVP

- Cron admission, Linear webhooks, and Hermes-triggered execution.
- Deployment, production operations, and destructive data changes.
- Automatic workflow or prompt evolution (Loop 4).
- Multi-repository transactions, multi-PR issues, and multi-tenant operation.
- Reimplementing Codex reasoning, context management, or memory.

## Engineering rules

- Use Go and keep dependencies minimal.
- Keep domain and application packages independent from CLI, Linear, GitHub,
  SQLite, and Codex process details.
- Treat external inputs as untrusted data, never as shell commands.
- Linear tasks may reference repository-owned verifier IDs only. Executable
  verification commands come from a controller-owned registry.
- Pass prompts through stdin. Never interpolate issue text into a shell string.
- Controller-managed Codex runs must ignore global user configuration so MCP,
  hooks, and tools cannot bypass controller-owned external side effects.
- Never use `--dangerously-bypass-approvals-and-sandbox`, `--ignore-rules`,
  `--skip-git-repo-check`, or `resume --last` in controller-managed runs.
- Capture Codex JSONL stdout separately from stderr.
- Validate the final message against the versioned JSON schema.
- Use a new empty artifact directory per attempt. Materialize schemas with
  exclusive creation and require Codex output leaf paths to be absent at spawn.
- Bind every verification and review result to the exact Git head SHA.
- Preserve unknown Codex JSONL event types as telemetry; do not make them fatal.
- Record and preflight the Codex CLI version and required flags. Do not use
  `--strict-config` to couple a run to unrelated global configuration fields.
- Make state transitions explicit, idempotent, persisted, and auditable.
- Do not add speculative abstractions. Every changed line must support a current
  contract, test, or documented MVP boundary.

## Documentation Governance

### Document count

- Do not add a new top-level Markdown document for a feature by default. Update
  the existing canonical document that owns the information.
- A standalone document is permitted only for a high-risk runbook, an
  irreversible operator procedure, or a formal architecture decision record.
- Do not create permanent documents for one issue, phase, milestone slice,
  implementation plan, or handoff. Temporary design and work breakdown belongs
  in the issue or pull request.
- Runbooks are exceptional operator procedures under `docs/runbooks/`; ADRs are
  accepted decisions under `docs/decisions/`. Existing files do not earn an
  exception merely because they already exist.

### Canonical ownership

```text
README.md             project entry, value, current capability, and quick start
docs/architecture.md  architecture, modules, state, authority, and invariants
docs/operations.md    human installation, configuration, commands, and recovery
docs/development.md   tests, fixtures, E2E, migrations, and contribution
docs/roadmap.md       product direction, milestone status, and non-goals
AGENTS.md             agent behavior and repository rules
```

Detailed task state and short-term implementation breakdown belong in the
repository's issue tracker, not in a second Markdown backlog.

### Prevent duplication

- Do not copy a complete explanation into multiple documents. Other documents
  may provide a short context-specific summary and link to the canonical
  section.
- When behavior changes, edit its canonical document instead of appending the
  same update to several files.
- If a change reveals duplicated or contradictory documentation, consolidate it
  in the same pull request.
- Keep README as an entry point, not a reference manual. Keep architecture
  responsibility-oriented, not a file-by-file catalog. Keep roadmap milestone-
  oriented, not an implementation checklist.

### Keep documentation synchronized with code

When changing:

- CLI commands or flags, check `docs/operations.md`;
- state topology, authority, evidence, or module boundaries, check
  `docs/architecture.md`;
- test scripts, fixtures, E2E, or migration practice, check
  `docs/development.md`;
- product capability or milestone status, check `README.md` and/or
  `docs/roadmap.md`;
- any operator-facing behavior, check `docs/operations.md`;
- an agent workflow or repository rule, check `AGENTS.md`.

Update only the documents whose canonical responsibility actually changed. Do
not add release-note-style entries to every document for a small implementation
change.

### No historical accumulation

- README and architecture describe current effective behavior. Do not append
  Phase N, schema N, issue N, or migration-by-migration history.
- Git history, issues, and pull requests retain implementation history.
- Preserve historical context in canonical docs only when it still changes
  current operation, compatibility, security, or migration behavior.
- Remove retired terminology and future-tense descriptions once the current
  implementation supersedes them.

### Documentation definition of done

For documentation-related changes:

- check internal Markdown links and anchors;
- search for references to deleted documents;
- search for retired terminology and obsolete commands;
- verify every CLI example against the current command router and flags;
- run the repository verification gate and `git diff --check`;
- confirm no credential, authorization header, absolute private path, personal
  identity, or unsanitized artifact evidence entered the repository.

## Verification

Run before publishing changes:

```sh
gofmt -w cmd internal
go test ./...
go vet ./...
```

Changes to command construction or structured contracts require focused tests.
