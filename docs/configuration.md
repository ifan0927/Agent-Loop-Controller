# Configuration and UI Boundary

## Canonical location

On macOS, production commands read
`~/Library/Application Support/agent-loop-controller/controller.json` when
`--config` is omitted. `ifan-loop config path` reports this path, and
`ifan-loop config init` creates a secret-free version 3 starter file using
exclusive creation. The final directory is mode `0700` and the configuration
file is mode `0600`.

The following files are separate from the configuration document:

```text
~/Library/Application Support/agent-loop-controller/
  controller.json
  controller.db
  secrets/linear-token
  secrets/github-app.pem
```

`controller.db` is workflow state and evidence, not editable configuration.
The PEM and Linear token are credentials and must never be included in
`controller.json`, SQLite, artifacts, logs, or UI responses.

## Version 3 document

Version 3 adds a disabled-by-default, local-only automatic Linear Todo
admission authority. It does not start polling, contact Linear, open a
credential source, create runs, or start a worker. Version 2 replaces the
production `repository_registry_file` reference with an
inline `repositories` array. The validation, canonicalization, profile digest,
path isolation, and GitHub authority checks are identical to the legacy
registry. Version 1 and version 2 documents remain readable for existing tests
and isolated legacy configurations; a version 2 document is equivalent to
automatic admission being disabled. New operator configurations should use
version 3.

The configuration contains non-secret references only:

- controller database location and Codex runtime policy;
- Linear endpoint, team, limits, and one exact credential-source reference:
  `secret://file/linear-token` for the controller-owned
  `secrets/linear-token` leaf, or the legacy explicit
  `secret://env/IFAN_LOOP_LINEAR_TOKEN` environment source;
- GitHub App IDs, selected-repository identity, permission switches, and the
  absolute path of an external PEM file;
- repository origin binding, local checkout/run/worktree roots, verifier IDs,
  and trusted operator identities.

The optional `automation.linear_todo_admission` object is an authority record,
not an execution switch. `enabled: false` may omit the remaining fields. When
enabled, it must pin the IFAN team UUID/key, exact Todo (`Todo`, `unstarted`)
and In Progress (`In Progress`, `started`) workflow states, bounded scheduler
timing and candidate/page limits, one active run, a fixed GitHub `User`
requester trusted by every configured repository profile, `local_outbox`
notification mode, and a credential-source reference. The credential reference
uses the same exact allowlist as the Linear profile and is never
resolved by `config validate` or `config inspect`.

`config inspect` emits only the enabled flag, configured limits, fixed
non-secret requester identity, and the existing stable repository/profile
digests plus the Linear credential source type (`file` or `environment`). It
never emits the automatic-admission credential reference, workflow
state IDs, configuration paths, or credential contents.

An empty starter document is deliberately not runnable. Add at least one
matching GitHub App profile and repository entry, then run `config validate`.
This prevents a placeholder profile or repository from becoming an implicit
delivery target.

`config init` creates `secrets/` with mode `0700`, but never creates, repairs,
chmods, or overwrites `secrets/linear-token`. Create that leaf separately as a
regular, single-link file owned by the controller user with mode `0600`; it may
contain one non-empty token line with at most one trailing LF. File credentials
are re-read for every Linear request, so an operator can rotate the leaf. The
loader and `config validate` / `config inspect` never read token bytes.
`ifan-loop config doctor` performs the runtime credential check and returns
only readiness or a generic warning, never a token, path, source ref, or
filesystem detail.

## Future Web UI

A Web UI should use the controller process as an authenticated backend and
should not read or write configuration files directly from the browser. The
backend may expose a sanitized configuration projection and a draft/validate/
apply operation. Applying a configuration must validate the entire document,
write a new non-symlink file atomically, and preserve the existing per-run
authority snapshot: changing a repository configuration must never change an
active run's target or permissions.

The UI's normal delivery action is a single explicit issue trigger backed by
the same long-lived `controller run` / `controller drive` driver as the CLI. It
should render a sanitized run timeline and the current human-facing gate, not a
row of buttons for push, PR creation, reconciliation, merge, and cleanup. The
controller derives those transitions from durable state and performs them
automatically. At `awaiting_human_approval`, the UI may link to the exact
GitHub PR and explain that I-Fan must approve there; it must not forge or submit
an approval. At `awaiting_human_decision` or `manual_intervention`, the UI may
collect a structured operator decision or direct the operator to a recovery
workflow after showing the immutable evidence.

The UI must not return PEM contents, Linear credentials, authorization headers,
absolute artifact paths, idempotency keys, or unsanitized SQLite evidence.
Credential rotation remains an operator action on the external credential
source. Low-level per-state commands remain a backend-only recovery/debug API,
never a browser-controlled normal delivery flow.
