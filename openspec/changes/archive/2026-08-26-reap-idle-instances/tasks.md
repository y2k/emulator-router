## 1. Activity stamp in the registry

- [x] 1.1 Add the last-activity field to `entry` (`last_seen`, seconds) and treat a missing or zero value as "idle forever"; verify a test that reads an entry written without the field reports unbounded idleness
- [x] 1.2 Add `touch(ctx, session, owner)`: find the session's own entry by session id alone — never by owner, since resolving the owner costs a walk up the process tree on every tool call — and rewrite it with the current time, doing nothing when the session owns no entry; verify a test that the stamp advances for the owner's entry and leaves other entries untouched
- [x] 1.3 Stamp the entry on every iteration of the readiness wait, in both the launch path and the reconnect path; verify a fake-clock test where readiness takes longer than the idle limit and the entry is still not idle when it becomes ready
- [x] 1.4 Stamp on a successful acquire, for a freshly launched instance and for one handed out again; verify a test that a repeated acquire advances the stamp

## 2. Idle sweep

- [x] 2.1 Add the hardwired idle limit (5 minutes) to `config` with no settings-file field; verify a test that a settings file naming that field is rejected with exit code 2 and a reason naming the field
- [x] 2.2 Add `sweepIdle(ctx)`: kill and drop every entry whose stamp is older than the limit, re-reading the entry immediately before killing, and touching neither `adb` nor the process table when nothing is stale; verify tests for a stale entry with a live owner and live instance being killed, a fresh entry surviving, an entry with no stamp being killed, and no external command being run when every entry is fresh
- [x] 2.3 Call `sweepIdle` from `reap` so the pre-acquire cleanup stays complete; verify a test that a stale entry is gone by the time the cap is checked and its slot is handed out
- [x] 2.4 Call `touch` and `sweepIdle` at the top of `PreToolUse` and `SessionEnd` handling, before the settings file is looked up; verify tests that a stale entry is swept when no settings file is found (empty output, exit 0), when the settings file is invalid (exit 2), for a read-only probe command, and for a denied command

## 3. Lifecycle log

- [x] 3.1 Add a log helper appending one line to `<Registry>/router.log` — time from the clock seam, instance, event, reason — ignoring write failures; verify tests that a line is appended with the faked time and that an unwritable log leaves the operation's outcome unchanged
- [x] 3.2 Give `killInstance` a reason argument and pass one at every call site (idle with the measured idle time, dead owner, dead instance, image mismatch, release, failed boot); verify a test that the idle sweep writes a line naming the instance, the idle time and the reason
- [x] 3.3 Log acquire outcomes: instance launched, instance still booting, and every refusal with its reason; verify a test that a refusal at the instance cap writes a line carrying that reason

## 4. Visibility and documentation

- [x] 4.1 Show idle time per entry in `status` and update the existing status tests to the new line format; verify `go test ./cmd/emulator-router -run TestStatus` passes
- [x] 4.2 Update `README.md`: the `status` output columns, the log file and what it records; verify the documented column list matches the implementation
- [x] 4.3 Run `make` (vet + test + build) and verify it is clean

## 5. Observation on real work

- [ ] 5.1 Run real sessions with the hook installed and verify `~/.claude/emulator-router/router.log` records the idle sweeps, checking in particular whether a long `connectedAndroidTest` run kills its own instance after the idle limit
