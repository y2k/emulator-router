## 1. Help text

- [x] 1.1 Add a help-text constant next to `dispatch()` in `cmd/emulator-router/main.go`: a one-screen listing of every subcommand with its arguments and purpose, naming `help`, `--help` and `-h` themselves, the default subcommand, the subcommands that need the project settings file, the hook exit codes, and the README URL. Verify by eye that it fits one screen and that no line describing configuration duplicates the README.
- [x] 1.2 Add a `case "help", "--help", "-h":` branch to the `dispatch()` switch that prints the constant to stdout and returns 0. Verify with `make run ARGS="--help"` and `make run ARGS="help"`.
- [x] 1.3 Shorten the `default:` branch to the unrecognized subcommand name plus a pointer to `--help`, written to stderr with exit code 1 — not the help text itself. Verify with `make run ARGS="nosuch"`.

## 2. Tests

- [x] 2.1 Add a test asserting each of `help`, `--help`, `-h` prints to stdout with exit code 0 and an empty stderr, and that a call with no arguments still runs `status` without printing help. Verify with `go test ./cmd/emulator-router -run TestHelp -v`.
- [x] 2.2 Add a test asserting an unrecognized subcommand exits 1, writes to stderr only, and names a way to get the help. Verify it fails when the `default:` branch is made to return 0.
- [x] 2.3 Add a completeness test that parses `cmd/emulator-router/main.go` with `go/parser`, collects the string case labels of the `dispatch()` switch, and requires every label to appear in the help text. Verify it fails when a case label is dropped from the help constant, and mark the source-reading approach with a `ponytail:` comment naming its ceiling (it knows the shape of `dispatch()`).

## 3. Documentation

- [x] 3.1 Add a `help` row to the subcommand table in `README.md` and check the remaining rows still match the help text word for word in meaning. Verify by reading both side by side.
- [x] 3.2 Run `make` and verify vet, tests, and build all pass.
