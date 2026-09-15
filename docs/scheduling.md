# Scheduling brw

brw has no scheduler and is not getting one. launchd, systemd and cron already
run things on a clock, survive reboots, hold the machine awake or not as you
prefer, and are where the rest of your jobs live. What brw owes them is a
contract: a command that never asks a question, a result a script can read, exit
codes that mean one thing each, and a guarantee that two runs cannot end up on
the same browser tab.

That contract is `brw run`.

```
brw run <recipe-id> --recipe-version <v> --digest <sha256> [--input k=v]...
```

It runs one recipe, prints one JSON object on stdout, writes human diagnostics
on stderr, and exits with a code from the table below. It never prompts and
never waits for input.

## Why a recipe and not a prompt

A scheduled job runs with nobody watching. A recipe is a pinned, immutable
document — id, version and digest — so the job that ran last night and the job
that runs tonight are the same steps. `--recipe-version` and `--digest` are both
required for that reason: without them a republished recipe would silently
change what your machine does at 3am. See
[recipes-and-artifacts.md](recipes-and-artifacts.md) for authoring one.

## Exit codes

| code | outcome | meaning |
|---|---|---|
| 0 | `ok` | the recipe ran and every step reached its asserted state |
| 1 | `failed` | the run failed for a reason that is none of the others; read error |
| 2 | `usage` | the invocation is wrong; the scheduler's command line needs fixing |
| 3 | `infrastructure` | brw could not run it: no daemon reachable, a transport failure, or a timeout |
| 4 | `postcondition_failed` | the recipe ran and a step did not reach its asserted state |
| 5 | `policy_refused` | site permissions or the confirmation gate refused; a human has to grant something |
| 6 | `busy` | another run holds this browser profile; nothing was attempted |

The distinction that matters to a wrapper is 4 versus 3. A postcondition failure
means the browser, the daemon and the machine are all fine and the work did not
land — worth another attempt on the next tick, and worth a human's attention if
it repeats. An infrastructure failure means brw never got to try. 5 is neither:
retrying it changes nothing until somebody grants a permission.

The JSON object carries the same decision as `outcome`, plus `retryable`, so a
wrapper does not have to hard-code this table:

```json
{
  "schema": "brw.run/1",
  "ok": false,
  "outcome": "postcondition_failed",
  "exit_code": 4,
  "retryable": true,
  "daemon": "http://127.0.0.1:17310",
  "profile": {"workspace": "work", "profile": "chrome-work", "lock_key": "5e1c…"},
  "recipe": {"id": "example.invoices.download", "version": "3", "digest": "dead…"},
  "started_at": "2026-09-15T03:00:00Z",
  "duration_ms": 41230,
  "lock_wait_ms": 0,
  "result": {"status": "failed", "steps": [{"id": "submit", "status": "failed"}]},
  "error": "step \"submit\": postcondition network_response did not occur",
  "diagnostic": "the recipe ran and a step did not reach its asserted state"
}
```

## One run at a time, per profile

Clocks overlap. A nightly job that usually takes four minutes takes twenty on
the night the site is slow, and the next morning's run starts while it is still
going. Both drive the same Chrome profile, their steps interleave on one tab,
and each run's trace contains the other's actions.

`brw run` takes a lock before it starts anything, keyed by the browser profile
the daemon reports at `/health` — the workspace, profile name, user data
directory and profile directory. It is keyed by the profile and not by the
daemon on purpose: an `--upstream-http` MCP proxy and the bridge daemon behind
it are two URLs driving one browser, and a per-daemon lock would let those two
interleave while each looked perfectly serialised. The proxy adopts the profile
of the daemon it forwards to, so both take the same key.

A daemon that names no profile at `/health` is refused rather than run. There is
no key that would be honest for one: it would take a lock shared with every
other anonymous daemon while an identified daemon on the same Chrome took the
profile's own, and the two would interleave on one tab while each reported a
lock key. Start the daemon with `--workspace`/`--profile`; `brwctl setup` does.

* `--lock-wait <duration>` (default `5m`) is how long to wait for the run in
  front. This is the queueing behaviour.
* `--lock-wait 0` refuses immediately with exit 6 instead. Use this when the
  next tick is soon and a backlog is worse than a skipped run.

The lock is an advisory file lock in your user cache directory. The kernel drops
it when the process dies, so a run killed mid-flight does not leave the profile
locked.

## It fails closed

An unattended run has nobody to ask, so anything that would ask is a refusal:

* An origin with no grant, an expired grant, or one in a blocked category exits
  5. Grant it ahead of time with `brwctl grants allow <origin> --scope act` and
  see [site-permissions.md](site-permissions.md).
* A high-risk action under `--confirm-actions` exits 5 rather than approving
  itself on your behalf.
* A daemon started with `--site-consent-prompt` is refused **before the run
  starts**, also with exit 5. That daemon would block on a terminal read nobody
  is going to answer, and a job that hangs to its timeout reports a timeout,
  which is not what happened. Run scheduled jobs against a daemon without the
  prompt.
* A daemon whose `/health` does not report a consent posture at all — one built
  before the block existed — is refused the same way. "No prompter" and "said
  nothing" are different answers, and only the first one is safe to act on.

These are the consent and confirmation surfaces brw already has. `brw run` adds
no policy of its own; it only reports their decision in a form a scheduler can
act on.

## launchd (macOS)

A per-user agent. Save it as
`~/Library/LaunchAgents/com.example.brw.invoices.plist`, then
`launchctl bootstrap gui/$(id -u) ~/Library/LaunchAgents/com.example.brw.invoices.plist`.

```xml
<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
  <key>Label</key>
  <string>com.example.brw.invoices</string>
  <key>ProgramArguments</key>
  <array>
    <string>/usr/local/bin/brw</string>
    <string>run</string>
    <string>example.invoices.download</string>
    <string>--recipe-version</string>
    <string>3</string>
    <string>--digest</string>
    <string>deadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeef</string>
    <string>--input</string>
    <string>account=example-ltd</string>
    <string>--lock-wait</string>
    <string>10m</string>
  </array>
  <key>EnvironmentVariables</key>
  <dict>
    <key>BRW_URL</key>
    <string>http://127.0.0.1:17310</string>
  </dict>
  <key>StartCalendarInterval</key>
  <dict>
    <key>Hour</key><integer>3</integer>
    <key>Minute</key><integer>0</integer>
  </dict>
  <key>StandardOutPath</key>
  <string>/usr/local/var/log/brw/invoices.jsonl</string>
  <key>StandardErrorPath</key>
  <string>/usr/local/var/log/brw/invoices.log</string>
  <key>ProcessType</key>
  <string>Background</string>
</dict>
</plist>
```

`StandardOutPath` collects the JSON objects, one per line, which is a ledger of
every run you can read with `jq`. `StandardErrorPath` collects the sentences.
Create the log directory first; launchd will not.

launchd does not report the exit code anywhere you will see it. Read the
outcome:

```sh
tail -n 1 /usr/local/var/log/brw/invoices.jsonl | jq -r '.outcome, .error'
```

`RunAtLoad` is deliberately absent: a job that runs the moment you log in will
fight the browser you are opening.

## systemd (Linux)

A user timer, so the job runs in your session and reaches the browser profile
you are signed into. Save both files under `~/.config/systemd/user/`, then
`systemctl --user enable --now brw-invoices.timer`.

`brw-invoices.service`:

```ini
[Unit]
Description=Download invoices with brw
Documentation=https://github.com/Don-Works/brw/blob/main/docs/scheduling.md

[Service]
Type=oneshot
Environment=BRW_URL=http://127.0.0.1:17310
ExecStart=/usr/local/bin/brw run example.invoices.download --recipe-version 3 --digest deadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeef --input account=example-ltd --lock-wait 10m
StandardOutput=append:%h/.local/state/brw/invoices.jsonl
StandardError=journal
# 4 is a postcondition that did not hold and 6 is another run holding the
# profile. Both are ordinary outcomes of a scheduled job, not unit failures, so
# they are not reported as a failed unit; 3 and 5 still are.
SuccessExitStatus=4 6
```

`brw-invoices.timer`:

```ini
[Unit]
Description=Download invoices with brw, nightly

[Timer]
OnCalendar=*-*-* 03:00:00
Persistent=true

[Install]
WantedBy=timers.target
```

`Persistent=true` runs a missed job once after the machine wakes up, rather than
skipping the night. Combined with `--lock-wait`, a catch-up run that collides
with the ordinary one queues behind it instead of interleaving.

Check the last outcome with `systemctl --user status brw-invoices.service`, or
read the JSON ledger the same way as on macOS.

## cron

cron works, with two caveats it will not warn you about: it runs with almost no
environment, so `BRW_URL` has to be set in the crontab, and it mails stderr to
the local user, which usually goes nowhere. Redirect both streams.

```crontab
0 3 * * * BRW_URL=http://127.0.0.1:17310 /usr/local/bin/brw run example.invoices.download --recipe-version 3 --digest deadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeef --input account=example-ltd --lock-wait 10m >> /usr/local/var/log/brw/invoices.jsonl 2>> /usr/local/var/log/brw/invoices.log
```

## Before the first scheduled run

1. The daemon has to be running and reachable at `BRW_URL`. `brwctl setup`
   installs it as a background service; `brwctl daemons` lists what is
   configured and probes each one.
2. The recipe has to be installed in the provider the daemon is configured with:
   `brwctl recipe install --file <path>`.
3. Every origin the recipe touches has to be granted, if the daemon runs with
   `--site-consent`: `brwctl grants allow https://example.test --scope act`.
4. Run it once by hand, exactly as the scheduler will:
   `BRW_URL=http://127.0.0.1:17310 brw run example.invoices.download
   --recipe-version 3 --digest <digest>`. An exit 5 here is the one you want to
   find now rather than at 3am.
