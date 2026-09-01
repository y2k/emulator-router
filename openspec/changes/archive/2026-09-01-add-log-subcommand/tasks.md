## 1. Reading the journal

- [x] 1.1 Add a journal reader next to `logDecision` in `cmd/emulator-router/pool.go`: it takes the window as a duration, reads `router.log` whole, finds the first line whose RFC3339 prefix parses to a moment at or after `cfg(ctx).Now()` minus the window, and returns that line and everything after it. A missing file yields empty output and no error. Mark the tail-scan approach with a `ponytail:` comment naming its ceiling (a line appended out of order within a second is included). Verify with the unit tests in 3.1.

## 2. Subcommand

- [x] 2.1 Add a `case "log":` branch to the `dispatch()` switch in `cmd/emulator-router/main.go`: parse the optional first positional argument as a non-negative integer number of minutes, defaulting to 30 minutes when absent, print the reader's output to stdout and return 0. Verify with `make run ARGS="log"` and `make run ARGS="log 5"` against a registry that has a journal.
- [x] 2.2 Reject a window argument that is not a non-negative integer: message to stderr, exit code 1, nothing on stdout. Do not reuse the `owner()` pattern that swallows the `strconv.Atoi` error. Verify with `make run ARGS="log abc"` and `make run ARGS="log -5"`.
- [x] 2.3 Add the `log [minutes]` row to `helpText`, next to `status`, naming the default window and the fact that it needs no project settings file. Verify `go test ./cmd/emulator-router -run TestHelpNamesEverySubcommand` passes.

## 3. Tests

- [x] 3.1 Add a test in `cmd/emulator-router/router_test.go` that writes a journal with records both inside and older than the window, using the `Now` seam, and asserts only the records inside the window are printed, in journal order, with exit code 0. Verify it fails when the cutoff comparison is inverted.
- [x] 3.2 Add a test asserting the default window applies when no argument is given, and that it is larger than `IdleTimeout` so a launch and the idle reap it triggers land in the same default window. Verify with `go test ./cmd/emulator-router -run TestLog -v`.
- [x] 3.3 Add a test asserting a non-numeric and a negative window each exit non-zero, write to stderr only, and print nothing to stdout. Verify it fails when the parse error is ignored.
- [x] 3.4 Add a test asserting a missing journal file prints nothing and exits 0, and that a line without a parseable timestamp sitting after the cutoff line is still printed. Verify with `go test ./cmd/emulator-router -run TestLog -v`.

## 4. Documentation

- [x] 4.1 Add a `log` row to the subcommand table in `README.md`, matching the help text in meaning. Verify by reading both side by side.
- [x] 4.2 Run `make` and verify vet, tests and build all pass.
