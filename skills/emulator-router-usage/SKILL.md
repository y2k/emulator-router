---
name: emulator-router-usage
description: Work with the Android emulator in a project connected to emulator-router - adb, app installs, Mobile-MCP calls, a hook denial "No emulator for this session", an installed app or device state that vanished. To connect a project or repair the connection use emulator-router-setup.
---

# Work with an emulator-router instance

The session owns one emulator instance, and the hook **pins** every device call
to it:

- a Bash command that reaches the device gets `ANDROID_SERIAL` and `ADB_SERIAL`
  exported, and every `adb -s <serial>` or `adb -e` rewritten to the session's
  serial;
- a Mobile-MCP call gets its `device` argument rewritten.

Write device commands without a serial: `adb install app.apk`, `adb shell ...`.
`adb devices` lists other sessions' instances too; the session's own one is the
serial named in the hook's context. The pool launches instances on demand, so the
first device call of a session may wait for a boot.

## The instance is temporary

- **Idle sweep.** After five minutes with no hook event at all from this session,
  its instance is killed and the slot goes back to the pool. The next device call
  gets a fresh instance from the boot snapshot: installed apps, logins, files and
  settings are gone. After any pause, check the app is installed before relying
  on it, and reinstall. `emulator-router log` confirms it with a line
  `kill reason=idle ... session=<id>`.
- **Session end** releases the instance the same way.

## Calls the hook does not see

The hook recognizes `adb`, `emulator`, `gradlew` install and connected-test
tasks, and the project's wrappers listed under `reach` in
`.claude/emulator-router.json` — each only as a command, not inside quoted text.
A project script missing from `reach` runs **unpinned**: it lands on whatever
device `adb` picks, possibly another session's, and nothing denies it. Symptoms:
"more than one device", or a result on a device that is not the session's.

The fix is a `reach` entry for that script (see emulator-router-setup). Until
then, put a recognized call first in the same command — the export covers the
whole command line: `adb wait-for-device && ./deploy.sh`.

In the main checkout the pool stays out by default (`mainCheckout`), and the
device there is the user's own.

## Denials

A denial starts with "No emulator for this session:" or names a forbidden
command. Respond by the reason:

| Reason | Response |
| --- | --- |
| `another session is launching an instance, retry in a moment` | Retry the same call. |
| `instance ... is still booting, retry in a moment` | Retry the same call. |
| `all N instances are busy` (followed by the pool status) | Tell the user the pool is full and show the status. Work with the pool: the busy instances belong to other sessions. |
| `instance on port ... did not boot in ...` | Report it with the log path it names; the user runs `emulator-router doctor`. |
| `AVD ... not found`, or a settings error blocking every call | The connection is broken: emulator-router-setup. |
| `Do not run 'adb root' here` | Run the single command as root: `adb shell su 0 <command>`. |
| `Do not launch "<avd>" without -read-only` | Leave launching to the pool; a device call gets an instance. Regenerating the snapshot is the user's job, from the main checkout. |

## Diagnose

- `emulator-router status` — held instances, owner liveness, idle time, working
  directory.
- `emulator-router log [minutes]` — the pool's launches, denials and kills in the
  last minutes (default 30), with reasons.
- `emulator-router help` — every subcommand.
