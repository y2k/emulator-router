## 1. Failing tests first

- [ ] 1.1 Add a failing test asserting that a session holding an instance of one image, asking again after the project settings name a different image, gets the old instance released and a new one of the requested image; verify it fails against the current lookup that ignores the image.
- [ ] 1.2 Add a failing test asserting an entry with no recorded image is treated as a different image and released on the next request, so entries written by earlier versions do not stick.

## 2. Implementation

- [ ] 2.1 Record the image in the registry entry when an instance is handed out; verify existing registry tests still pass, `TestEntrySurvivesSpacesInPaths` included.
- [ ] 2.2 Require the recorded image to match the requested one in the session lookup, releasing the instance when it does not; verify tasks 1.1 and 1.2 pass and `TestAcquireIdempotentAndIsolated` still passes.

## 3. Verification

- [ ] 3.1 Run `make` and confirm vet, the full suite and the build are green.
- [ ] 3.2 With two prepared AVDs, hand out an instance, change the image in the project settings, request again, and confirm the serial belongs to an instance of the new image while the old instance is gone from both the registry and the device list.
