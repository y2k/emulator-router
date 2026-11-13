## 1. Implementation

- [x] 1.1 Add `cmdStart` in `cmd/emulator-router/main.go` (line start or separator, then optional `NAME=value` assignments) and build `commandPos` on it.
- [x] 1.2 Build `rootRe` and `launchRe` in `cmd/emulator-router/hook.go` on `cmdStart`.
- [x] 1.3 Add assignment-prefixed cases to `TestReachRecognition`, `TestConfiguredReachKeepsCommandPosition`, `TestDenyAdbRoot` and `TestDenyWritableLaunchOfPooledAVD`, including a quoted assignment that must not be recognized.
- [x] 1.4 Mention environment assignments in the command-position paragraph of `README.md`.
- [x] 1.5 Run `make` and verify vet, tests, and build all pass.
