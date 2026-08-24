# emulator-router

A pool of Android emulators that gives every Claude Code session its own
instance: private data, private app install, state thrown away when the session
ends.

One binary does both jobs — routing hook events and managing the pool. A session
never has to know its own serial: shell commands get it substituted in and
exported, Mobile-MCP calls get their `device` argument rewritten.

Requires macOS or Linux, an Android SDK, and a prepared AVD with a boot
snapshot. Windows is not supported.

## Install

```sh
go install github.com/y2k/emulator-router/cmd/emulator-router@latest
```

Then wire the hooks in your project's `.claude/settings.json`, using the
absolute path printed by `go env GOPATH` plus `/bin/emulator-router`:

```json
{
  "hooks": {
    "PreToolUse": [
      {
        "matcher": "Bash|mcp__mobile-mcp__.*",
        "hooks": [{ "type": "command", "command": "/Users/you/go/bin/emulator-router hook", "timeout": 540 }]
      }
    ],
    "SessionEnd": [
      {
        "hooks": [{ "type": "command", "command": "/Users/you/go/bin/emulator-router hook", "timeout": 540 }]
      }
    ]
  }
}
```

The `timeout` is set on purpose. Handing out an instance can wait for another
session's launch and then for a boot, and the router gives up and denies the call
before its own budget runs out. A hook killed by the timeout does *not* block the
call — it would run unrouted, on whatever device came first — so the timeout is
kept above that budget rather than left at the default.

## Configure

The pool serves a project only when it finds `.claude/emulator-router.json`,
searching upward from the working directory of the call. No file means "not my
project": the tool call runs untouched. This is what makes a globally installed
hook harmless in unrelated repositories.

The search stops at the repository boundary, so a stray file in some parent
directory never applies to your project. Settings belong to the repository rather
than to one checkout: a linked worktree with no file of its own uses the main
checkout's, wherever the worktree lives on disk. A file inside the worktree wins
over the repository's.

```json
{
  "avd": "my_agent_avd",
  "max": 3,
  "gpu": "host",
  "reach": ["deploy\\.sh", "tools[^ ]*\\.kts"],
  "mainCheckout": false
}
```

| Field | Default | Meaning |
| --- | --- | --- |
| `avd` | — | **Required.** Name of the prepared AVD the pool clones. No default exists. |
| `max` | `3` | Cap on simultaneously held instances. |
| `gpu` | `host` | Value passed to `emulator -gpu`. Some Linux hosts need `swiftshader_indirect`. |
| `reach` | `[]` | Extra command words that reach the emulator (see below). |
| `mainCheckout` | `false` | Serve the main checkout too, not just linked worktrees. |

A found-but-unusable file is a hard error: unparsable JSON, a missing `avd`, an
unknown field, a wrong type, or an invalid `reach` pattern all block the tool
call with exit code 2. A typo in a field name must not be indistinguishable from
a working setting. Releasing an instance on `SessionEnd` does not need the file
and happens regardless.

Everything else — the registry directory (`~/.claude/emulator-router`), the
console port range, all timeouts — is not project configuration and is not
settable. The SDK location comes from `ANDROID_HOME` or `ANDROID_SDK_ROOT`, and
otherwise from the platform's standard location.

### What counts as reaching the emulator

Recognition requires the program name to sit in *command position* — at the start
of the line or after a command separator — so the word `emulator` inside quoted
prose does not acquire an instance nobody asked for.

The built-in core is `adb`, `emulator`, and `gradlew` in its install and
connected-device modes. Your own wrapper scripts go in `reach`: each entry is a
regular expression matched against **one command word**, and the router adds the
command-position requirement itself, so a careless pattern cannot weaken the
guarantee above.

**The recognized set is deliberately incomplete.** A wrapper you forget to list
is not blocked — it runs unpinned, against whatever device `adb` picks. If a
project script reaches a device, list it.

Two commands are refused outright: `adb root`, because restarting adbd knocks
every pooled instance offline including other sessions', and a writable launch of
the pooled AVD, because that destroys the boot snapshot every instance loads
from.

## Subcommands

| Subcommand | What it does |
| --- | --- |
| `hook` | handles a hook event on stdin: `PreToolUse` and `SessionEnd` |
| `acquire <session> [owner] [cwd]` | hands out an instance and prints its serial |
| `doctor [cwd]` | checks the preconditions the pool depends on |
| `release <session> [owner]` | frees the session's instance |
| `reap` | clears orphaned registry entries and copy-on-write layers |
| `status` | prints held instances, owner liveness, and owner working directory |

`hook` exit codes: `0` — success, decision (if any) on stdout; `2` — blocking
error, reason on stderr; `1` — release failure on `SessionEnd`, where there is
nothing left to block. The exit code that lets a tool call through is never used
for internal errors: an unpinned call would land on an arbitrary device, possibly
another session's.

`status` and `reap` work anywhere — they only touch the registry, never the AVD.
`acquire` and `doctor` need the settings file, since they are run by hand for a
specific directory.

## Diagnosing a broken setup

`doctor` separates a broken environment from broken code. It reports every check
separately and distinguishes violations from warnings: a non-zero exit means
something makes the pool unusable, while a warning means it works but worse.

Static checks come first — SDK tooling is executable, the settings file is found
and usable, the hook itself is wired into the agent's settings, the AVD exists, its boot snapshot is present both as saved memory and
*inside every base image* (a writable launch wipes the latter while the former
still looks fine), quick boot is requested, and there is disk room for the whole
cap. Then it boots one real instance to check what cannot be established
statically: that the snapshot actually loads instead of a cold boot, that adbd is
not running as root (a snapshot saved that way makes every clone fight over one
adb identity), and that root for a single command works.

If a static check already failed, the live checks are skipped — booting an
instance in a known-broken environment costs minutes and says nothing new. The
probe instance is released whatever the outcome.

A missing settings file is a violation here, deliberately: during hook handling
the same situation is silent by design, so this is the only place that can say
"this project is not configured" out loud. A missing hook is the other half of the
same silence — settings present, nothing calling the router — and every other
check, including the live one, still passes. Both are violations for that reason.

## Checks

```sh
make                        # vet + test + build
make test                   # fast: no emulators, no registry access
make emulators AVD=my_avd   # tests against real instances
```

`make test` starts no emulators, touches no real SDK tooling, and never reads the
real registry: the outside world is faked and the registry directory is temporary.

`make emulators` does the opposite — it boots real instances to verify isolation,
idempotent re-acquisition, the cap, and release. It needs a prepared AVD with a
boot snapshot, and it holds pool slots for minutes, so run it when the pool is
idle. It uses its own registry and its own port range, so it will not disturb a
running session; the AVD name is required rather than defaulted, and the run fails
loudly if it is missing instead of skipping silently — a skipped test reads as a
passing one.

## How it works

A registry entry is a JSON file named after the instance's console port. Launches
are serialized with a file lock (`flock`): the kernel releases it however the
process dies, so "the pool is wedged forever" is unreachable.

Reaping only ever touches instances present in this registry — emulators you
started yourself are left alone.

## Specs

Behaviour is specified in `openspec/` (in Russian): `specs/` is current
behaviour, `changes/` holds proposed changes. Behaviour changes go through a
change in `changes/`, not by editing a spec directly.

## License

GNU General Public License, version 2. See [LICENSE](LICENSE).
