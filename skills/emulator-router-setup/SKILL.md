---
name: emulator-router-setup
description: Connect a project to the emulator-router pool of Android emulators, or repair a connection that misbehaves - the hook never fires, every call is denied, instances cold boot. For device work in an already connected project use emulator-router-usage.
---

# Connect a project to emulator-router

emulator-router gives every Claude Code session its own Android emulator, cloned
from one prepared AVD. It runs as a hook: it pins device calls to the session's
instance and denies the ones that would hurt the pool. Connecting is done when
`emulator-router doctor` exits 0 in the project directory, and only then.

`emulator-router help` is the reference for subcommands and exit codes;
`emulator-router doctor` names each failed precondition with its fix. When
repairing, run `doctor` first and fix what it reports, in order.

## 1. Install

```sh
go install github.com/y2k/emulator-router/cmd/emulator-router@latest
```

The hook command needs the absolute path: `$(go env GOPATH)/bin/emulator-router`.
Requires macOS or Linux and an Android SDK (`ANDROID_HOME`, `ANDROID_SDK_ROOT`,
or the platform's standard location).

## 2. Wire the hooks

Into the project's `.claude/settings.json` (or `settings.local.json`, or the user's
`~/.claude/settings.json` for every project), with the real absolute path:

```json
{
  "hooks": {
    "PreToolUse": [
      {
        "matcher": "Bash|mcp__mobile-mcp__.*",
        "hooks": [{ "type": "command", "command": "/ABS/PATH/emulator-router hook", "timeout": 540 }]
      }
    ],
    "SessionEnd": [
      {
        "hooks": [{ "type": "command", "command": "/ABS/PATH/emulator-router hook", "timeout": 540 }]
      }
    ]
  }
}
```

Keep `"timeout": 540` on both. Handing out an instance can wait up to 480 s for
another launch and a boot, and a hook killed by its timeout does not block the
call: the call runs unpinned, on whatever device comes first, possibly another
session's. `SessionEnd` is what returns the instance to the pool.

A globally wired hook is harmless elsewhere: it stays silent in any project
without the settings file below.

## 3. Prepare the AVD

The pool clones one AVD and never creates it. Use a **Google APIs** image
(`google_apis`, not `google_apis_playstore`): Play images have no `su`, and
`doctor` needs root for one command. Pick the API level the app targets; this
example is API 36 on Apple silicon.

```sh
sdkmanager "emulator" "system-images;android-36;google_apis;arm64-v8a"
avdmanager create avd -n agent_base -k "system-images;android-36;google_apis;arm64-v8a" -d pixel_3a
```

In `~/.android/avd/agent_base.avd/config.ini`:

```ini
fastboot.forceColdBoot=no
fastboot.forceFastBoot=yes
hw.gpu.enabled=yes
hw.gpu.mode=host
hw.ramSize=4096
```

Boot it once writable, with the pool's own arguments minus `-read-only` so the
snapshot records the same renderer, and save the boot snapshot:

```sh
emulator @agent_base -port 5554 -gpu host -no-window -no-audio -no-boot-anim &
adb -s emulator-5554 wait-for-device
until [ "$(adb -s emulator-5554 shell getprop sys.boot_completed | tr -d '\r')" = 1 ]; do sleep 1; done
adb -s emulator-5554 emu avd snapshot save default_boot
adb -s emulator-5554 emu kill
```

Save with adbd unrooted: a snapshot taken after `adb root` makes every clone fight
over one adb identity. Whatever the guest holds at the save is what every instance
starts from. From now on the AVD launches only with `-read-only`; a writable
launch destroys the snapshot, and the hook denies one inside a connected project.

If the hook is already wired, run these from a directory without the settings
file, or before writing it — inside a connected project the hook rewrites `-s`.

## 4. Write the settings file

`.claude/emulator-router.json` at the repository root:

```json
{
  "avd": "agent_base",
  "reach": ["deploy\\.sh", "tools[^ ]*\\.kts"],
  "mainCheckout": false
}
```

- `avd` — required, the AVD from step 3.
- `reach` — regular expressions, each matched against one command word, naming
  the project's own scripts that reach a device. The built-in set covers `adb`,
  `emulator`, and `gradlew` install and connected-test tasks. List every wrapper
  that touches a device: an unlisted one runs unpinned, with no denial to warn
  anyone.
- `mainCheckout` — the pool serves linked worktrees only; `true` serves the main
  checkout too. Leave `false` when the main checkout is where a human uses their
  own device.

These three fields are the whole file. The instance cap and the renderer are
fixed by the binary, and any other field — those two included — makes the file
unusable, which blocks every device call with exit code 2.

The file is found by searching upward from the call's directory, stopping at the
repository boundary; a linked worktree without its own file uses the main
checkout's.

## 5. Verify

```sh
emulator-router doctor
```

Run it in the project directory (or pass the directory as its argument). It
checks the tooling, the settings file, the hook wiring, the AVD and its snapshot,
disk room, and then boots one real instance to confirm the snapshot loads. Fix
each violation it names and rerun until it exits 0; warnings mean the pool works,
but worse. Connected means exit 0 — every other state is not connected yet.
