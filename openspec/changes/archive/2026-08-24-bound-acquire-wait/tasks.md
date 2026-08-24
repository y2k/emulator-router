## 1. Boot-aware lookup

- [x] 1.1 Add a failing test asserting that a session's lookup leaves a booting instance alone: a fresh entry with a live instance process and a device that is not yet in a working state must not be killed, and its record must survive; verify it fails against the current kill-after-recovery-timeout behavior.
- [x] 1.2 Add a failing test asserting a repeated lookup during boot returns the same instance without starting a second one.
- [x] 1.3 Split the lookup's unavailability handling into the booting case (entry younger than the boot allowance with a live process → keep waiting within the boot allowance) and the genuinely offline case (existing behavior); verify tests 1.1 and 1.2 pass and `TestOfflineInstanceWithLiveProcessIsRecovered` and `TestDeadInstanceEntryIsNotHandedBack` still pass.

## 2. Early abort on a dead instance

- [x] 2.1 Add a failing test asserting readiness waiting stops as soon as the instance process is gone, well before the boot allowance elapses, and reports the launch log.
- [x] 2.2 Check for the instance process inside the readiness wait and abort early; verify task 2.1 passes and `TestAcquireFailsWhenBootTimesOut` and `TestBootRequiresTwoConsecutiveConfirmations` still pass.

## 3. Acquire budget

- [x] 3.1 Add a failing test asserting that when the total wait reaches the acquire budget the call is denied with a reason that names the retry, while a boot already in progress is neither killed nor its record dropped.
- [x] 3.2 Add the budget to `config` as a fixed value with a margin under the caller's 600s default, and document in the field comment why it is not a project setting; verify `go vet ./...` is clean.
- [x] 3.3 Compute a deadline once on entry to acquire and honour it in the lock wait, the recovery wait and the readiness wait; verify task 3.1 passes and the full suite is green.
- [x] 3.4 Verify the denial reason reaches the caller through the existing "instance could not be handed out" branch rather than a new one, by asserting on the decision a hook event produces.

## 4. Wiring and verification

- [x] 4.1 Set an explicit `timeout` on both hook entries in the `README.md` example, chosen above the acquire budget, and state in one sentence why it is set.
- [x] 4.2 Run `make` and confirm vet, the full suite and the build are green.
- [x] 4.3 With a prepared AVD, exhaust the budget on purpose (occupy the launch lock from a second acquire) and confirm the hook returns a denial with the retry reason instead of being killed, and that the instance booting under the lock survives and is handed to the next call.
