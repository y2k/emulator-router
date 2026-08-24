## 1. Whole-or-nothing registry writes

- [x] 1.1 Add a failing test asserting that a registry entry is never observable in a truncated state: write an entry over an existing one and assert that a reader either sees the previous complete entry or the new complete one; verify it fails against the current truncate-then-write behavior.
- [x] 1.2 Write entries through a temporary file renamed into place; verify task 1.1 passes and `TestEntrySurvivesSpacesInPaths` still passes.

## 2. Unreadable entries stop being immortal

- [x] 2.1 Add a failing test asserting that reaping removes an entry that cannot be read once its file has not changed for longer than the boot allowance, freeing both the cap slot and the port.
- [x] 2.2 Add a failing test asserting that a just-written unreadable entry is spared, so an entry being created right now is not swept.
- [x] 2.3 Handle unreadable entries in reaping using the file modification time as the grace reference; verify tasks 2.1 and 2.2 pass and `TestReapSparesBootingEntry` and `TestReapRemovesOrphans` still pass.

## 3. Layer sweep stops guessing

- [x] 3.1 Add a failing test asserting that when the occupancy check tool is absent the copy-on-write layer is kept, not deleted; verify it fails against the current behavior that treats any error as "free".
- [x] 3.2 Distinguish "not occupied" from "cannot tell" in the layer sweep; verify task 3.1 passes and `TestReapSweepsUnusedCOWLayers` still passes.

## 4. Doctor's probe entry

- [x] 4.1 Add a failing test asserting the probe entry is owned by the checking process, so that reaping collects it once that process is gone; verify it fails against the current undefined owner.
- [x] 4.2 Pass the checking process as the owner of the probe instance; verify task 4.1 passes and `TestDoctorReleasesProbeInstanceOnFailure` still passes.

## 5. Verification

- [x] 5.1 Run `make` and confirm vet, the full suite and the build are green.
- [x] 5.2 With a prepared AVD, interrupt `doctor` between acquire and release, then run reaping and confirm the leaked slot and its instance are reclaimed.
