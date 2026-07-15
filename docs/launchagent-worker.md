# macOS LaunchAgent worker runbook

This runbook supervises one logged-in user's already-built controller worker.
It is not a LaunchDaemon, HTTP service, webhook, tunnel, package installer, or
multi-user deployment. launchd restarts only an unsuccessful worker exit; the
SQLite scheduler lease and admission journal remain the authority for restart
and recovery.

## Prepare the local files

Install a release binary outside a repository checkout. The examples use the
current-user-managed `~/.local/bin/ifan-loop`; it must be an absolute,
canonical executable path.
The doctor also requires that the binary is a current-user-owned, non-symlink
regular file with an execute bit and no group/world write bit. `controller.json`
must be a current-user-owned non-symlink regular file at exactly mode `0600`.
Use the standard controller configuration location and create the log directory
and leaves before launchd opens them:

```sh
CONFIG="$HOME/Library/Application Support/agent-loop-controller/controller.json"
LOG_DIR="$HOME/Library/Application Support/agent-loop-controller/logs"
PLIST="$HOME/Library/LaunchAgents/com.ifan.agent-loop-controller.worker.plist"
BIN="$HOME/.local/bin/ifan-loop"

mkdir -p "$LOG_DIR"
chmod 700 "$LOG_DIR"
touch "$LOG_DIR/worker.stdout.log" "$LOG_DIR/worker.stderr.log"
chmod 600 "$LOG_DIR/worker.stdout.log" "$LOG_DIR/worker.stderr.log"
```

The Linear file credential remains the separately managed
`secrets/linear-token` leaf. It is never put in a plist, shell startup file,
environment variable, log, or rendered output. Its existing `0700` directory
and `0600` regular single-link token-file requirements still apply.

## Build, validate, and install safely

Build the release binary outside the controller repository as an operator
step. The controller's `build`/`render` step builds only the exact plist
document; it never builds a binary or installs a package. The plist install is
absent-only and uses exclusive creation. It is idempotent for the exact same
document and refuses to overwrite an existing or unrelated plist:

```sh
go build -o "$BIN" ./cmd/ifan-loop
"$BIN" controller launchagent doctor --binary "$BIN" --config "$CONFIG"
"$BIN" controller launchagent validate --binary "$BIN" --config "$CONFIG" --plist "$PLIST"

tmp="$(mktemp "$HOME/Library/LaunchAgents/.agent-loop-worker.XXXXXX")"
"$BIN" controller launchagent build --binary "$BIN" --config "$CONFIG" --plist "$PLIST" > "$tmp"
plutil -lint "$tmp"
test ! -e "$PLIST"
chmod 600 "$tmp"
"$BIN" controller launchagent install --binary "$BIN" --config "$CONFIG" --plist "$PLIST"
"$BIN" controller launchagent plist-validate --binary "$BIN" --config "$CONFIG" --plist "$PLIST"
rm -f "$tmp"
```

`doctor` and `validate` are read-only and return only finite reason codes; they
do not repair permissions, create logs, read credentials into output, or
replace a plist. `validate` reports `plist_exists` rather than overwriting any
existing user or unrelated LaunchAgent. `plist-validate` checks the exact
label, `controller worker` argv, and `RunAtLoad` contract without invoking
`launchctl`.

The versioned template has a stable label, exact `controller worker` argv,
`RunAtLoad`, `KeepAlive.SuccessfulExit=false`, a 30-second throttle, and umask
`0077` (plist decimal `63`). A normal worker stop such as operator attention is
a successful exit and is not restarted indefinitely. There are no shell,
`go run`, checkout, token, requester, GitHub key, issue, branch, idempotency,
or environment entries.

## Bootstrap, observe, and recover

Each control operation is a separate bounded step. The default per-step
timeout is 15 seconds; use `--timeout` for a shorter or longer value up to
two minutes. The controller emits only the exact label, a finite observed
state, a finite outcome, a next safe action, and (when relevant) a reason code.
It never prints `launchctl` stdout/stderr, paths, credentials, or task data.

For the current GUI user, bootstrap reuses an already loaded service and
performs a status read after a new bootstrap:

```sh
"$BIN" controller launchagent bootstrap --binary "$BIN" --config "$CONFIG" --plist "$PLIST"
"$BIN" controller launchagent status --binary "$BIN" --config "$CONFIG" --plist "$PLIST"
"$BIN" controller launchagent kickstart --binary "$BIN" --config "$CONFIG" --plist "$PLIST"
tail -n 100 "$LOG_DIR/worker.stdout.log"
tail -n 100 "$LOG_DIR/worker.stderr.log"
```

`kickstart` first observes the service. It does nothing when the service is
already running or when `RunAtLoad=true` is still in launchd's initial
`loaded`, `waiting`, or `scheduled` state. A stable `stopped` or `exited`
service is explicitly kickstarted once; `RunAtLoad` does not retrigger after a
worker has already exited. A timed-out control step reports operator attention
and does not assume success or issue a second control command. `status` remains
the observation source for the next manual decision.

To stop it without deleting the plist, use the idempotent bootout step:

```sh
"$BIN" controller launchagent bootout --binary "$BIN" --config "$CONFIG" --plist "$PLIST"
"$BIN" controller launchagent status --binary "$BIN" --config "$CONFIG" --plist "$PLIST"
```

If the service is already absent, `bootout` reports `already_stopped` and does
not invoke a second stop. The exact label is the only service target; no
compound shell recipe is authoritative.

After an unexpected crash, launchd may restart the worker after the throttle.
That never bypasses the worker's credential preflight or replaces the SQLite
lease/journal; on restart the dispatcher resumes one non-terminal run before
scanning Todo. Unsafe configuration, credential, database-parent, or log
permissions keep the worker stopped and require local operator correction.

## Upgrade, uninstall, and logs

For an upgrade, first run the bounded `bootout` step, independently install
and verify the new binary, re-run the read-only doctor/validate checks, build a
new temporary plist, run `plutil -lint`, and use `install` only after the
operator has deliberately removed the exact old plist. Never overwrite an
unknown existing plist; `install` will stop with `plist_exists` instead.

For uninstall, run the bounded `bootout` step, confirm `status` reports the
exact label absent, then remove only that file. Keep controller state, secrets,
and logs unless the operator explicitly decides otherwise.

The unit and contract tests fake the `launchctl` port and verify argv, timeout,
state decisions, idempotence, and sanitized output. A real `launchctl`
bootstrap/status/kickstart/bootout smoke is intentionally a macOS-only,
logged-in-user check; CI cannot prove launchd behaviour and the controller
does not claim that it can.

Logs contain only controller-sanitized output, but remain private. Rotate them
only while the agent is booted out: retain at most seven generations and at
most 5 MiB per generation, then create fresh `0600` leaves before bootstrap.
Do not truncate an active launchd-owned log and do not copy logs to shared or
cloud-synced locations.
