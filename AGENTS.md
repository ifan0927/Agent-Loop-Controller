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

## Verification

Run before publishing changes:

```sh
gofmt -w cmd internal
go test ./...
go vet ./...
```

Changes to command construction or structured contracts require focused tests.
