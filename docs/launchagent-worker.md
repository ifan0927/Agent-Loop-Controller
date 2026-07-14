# macOS LaunchAgent worker runbook

This runbook supervises one logged-in user's already-built controller worker.
It is not a LaunchDaemon, HTTP service, webhook, tunnel, package installer, or
multi-user deployment. launchd restarts only an unsuccessful worker exit; the
SQLite scheduler lease and admission journal remain the authority for restart
and recovery.

## Prepare the local files

Install a release binary outside a repository checkout. The examples use
`/usr/local/bin/ifan-loop`; it must be an absolute, canonical executable path.
The doctor also requires that the binary is a current-user-owned, non-symlink
regular file with an execute bit and no group/world write bit. `controller.json`
must be a current-user-owned non-symlink regular file at exactly mode `0600`.
Use the standard controller configuration location and create the log directory
and leaves before launchd opens them:

```sh
CONFIG="$HOME/Library/Application Support/agent-loop-controller/controller.json"
LOG_DIR="$HOME/Library/Application Support/agent-loop-controller/logs"
PLIST="$HOME/Library/LaunchAgents/com.ifan.agent-loop-controller.worker.plist"
BIN=/usr/local/bin/ifan-loop

mkdir -p "$LOG_DIR"
chmod 700 "$LOG_DIR"
touch "$LOG_DIR/worker.stdout.log" "$LOG_DIR/worker.stderr.log"
chmod 600 "$LOG_DIR/worker.stdout.log" "$LOG_DIR/worker.stderr.log"
```

The Linear file credential remains the separately managed
`secrets/linear-token` leaf. It is never put in a plist, shell startup file,
environment variable, log, or rendered output. Its existing `0700` directory
and `0600` regular single-link token-file requirements still apply.

## Render and validate without installation

The controller never writes a LaunchAgent plist. Render to a temporary file,
validate it, and verify that a target plist is absent before any manual move:

```sh
"$BIN" controller launchagent doctor --binary "$BIN" --config "$CONFIG"
"$BIN" controller launchagent validate --binary "$BIN" --config "$CONFIG" --plist "$PLIST"

tmp="$(mktemp "$HOME/Library/LaunchAgents/.agent-loop-worker.XXXXXX")"
"$BIN" controller launchagent render --binary "$BIN" --config "$CONFIG" > "$tmp"
plutil -lint "$tmp"
test ! -e "$PLIST"
chmod 600 "$tmp"
mv "$tmp" "$PLIST"
```

`doctor` and `validate` are read-only and return only finite reason codes; they
do not repair permissions, create logs, read credentials into output, or
replace a plist. `validate` reports `plist_exists` rather than overwriting any
existing user or unrelated LaunchAgent.

The versioned template has a stable label, exact `controller worker` argv,
`RunAtLoad`, `KeepAlive.SuccessfulExit=false`, a 30-second throttle, and umask
`0077` (plist decimal `63`). A normal worker stop such as operator attention is
a successful exit and is not restarted indefinitely. There are no shell,
`go run`, checkout, token, requester, GitHub key, issue, branch, idempotency,
or environment entries.

## Load, inspect, and recover

For the current GUI user:

```sh
uid="$(id -u)"
launchctl bootstrap "gui/$uid" "$PLIST"
launchctl kickstart -k "gui/$uid/com.ifan.agent-loop-controller.worker"
launchctl print "gui/$uid/com.ifan.agent-loop-controller.worker"
tail -n 100 "$LOG_DIR/worker.stdout.log"
tail -n 100 "$LOG_DIR/worker.stderr.log"
```

To stop it without deleting the plist:

```sh
launchctl bootout "gui/$(id -u)" "$PLIST"
```

After an unexpected crash, launchd may restart the worker after the throttle.
That never bypasses the worker's credential preflight or replaces the SQLite
lease/journal; on restart the dispatcher resumes one non-terminal run before
scanning Todo. Unsafe configuration, credential, database-parent, or log
permissions keep the worker stopped and require local operator correction.

## Upgrade, uninstall, and logs

For an upgrade, first `bootout`, install and independently verify the new
binary, re-run the read-only doctor/validate checks, render a new temporary
plist, run `plutil -lint`, then manually replace only this exact label's plist
and `bootstrap`/`kickstart` it again. Never overwrite an unknown existing
plist.

For uninstall, `bootout` the exact GUI-label plist, confirm it is the expected
label, then remove only that file. Keep controller state, secrets, and logs
unless the operator explicitly decides otherwise.

Logs contain only controller-sanitized output, but remain private. Rotate them
only while the agent is booted out: retain at most seven generations and at
most 5 MiB per generation, then create fresh `0600` leaves before bootstrap.
Do not truncate an active launchd-owned log and do not copy logs to shared or
cloud-synced locations.
