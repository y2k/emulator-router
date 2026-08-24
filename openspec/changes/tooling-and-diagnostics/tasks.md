## 1. Build version in the report

- [ ] 1.1 Add a failing test asserting the report names the build version, and names it explicitly as absent when the build carries none; verify it fails against the current report.
- [ ] 1.2 Add a version value settable at build time, defaulting to an explicit "unknown" rather than an empty string, and report it among the static checks; verify task 1.1 passes and `TestDoctorReportsEachCheckSeparately` still passes.
- [ ] 1.3 Stamp the version in the `Makefile` build target from the git description and verify `bin/emulator-router doctor` prints it.

## 2. Checks that run by default

- [ ] 2.1 Add race detection to the test target and verify `make test` passes with it enabled.
- [ ] 2.2 Add a formatting check that fails instead of rewriting, wire it into `make` and the CI workflow, and verify it fails on a deliberately misformatted file and passes on the clean tree.

## 3. One device listing per request

- [ ] 3.1 Take the device listing once per acquire and reuse it for the session lookup and the free-port check; verify the full suite passes, `TestAcquirePicksPortsInStepsOfTwo` and `TestDeadInstanceEntryIsNotHandedBack` included.
- [ ] 3.2 If the change turns out to cost more code than it saves calls, drop it and record that in the change instead of forcing it through.

## 4. Verification

- [ ] 4.1 Run `make` and confirm vet, formatting, the race-enabled suite and the build are all green.
