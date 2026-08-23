## 1. Failing tests first

- [x] 1.1 Extend `TestMobileMCPDeviceRewrite` with a `tool_input` that carries required non-string arguments (numeric `x`/`y`, boolean `submit`) and assert every one of them reaches `updatedInput` with its original value and type while `device` is replaced; verify the new assertions fail against the current code with `go test ./cmd/emulator-router -run TestMobileMCPDeviceRewrite`.
- [x] 1.2 Extend `TestBashSubstitution` with `run_in_background` and `timeout` in `tool_input` and assert both reach `updatedInput` unchanged while `command` is overridden; verify it fails first with `go test ./cmd/emulator-router -run TestBashSubstitution`.
- [x] 1.3 Add a case asserting that an input with no fields beyond the overridden one produces an `updatedInput` holding only that field, so the fix cannot invent defaults; verify it fails or passes for the right reason before the implementation lands.

## 2. Implementation

- [x] 2.1 Change `hookSpecific.UpdatedInput` to `map[string]any` so non-string arguments survive marshalling; verify `go build ./...` and `go vet ./...` are clean.
- [x] 2.2 In the Mobile-MCP branch of `hook`, build the decision input as a copy of `in.ToolInput` with `device` overridden, leaving the parsed input untouched; verify task 1.1 now passes.
- [x] 2.3 In `bashCall`, build the decision input as a copy of `in.ToolInput` with `command` overridden; verify tasks 1.2 and 1.3 now pass.

## 3. Verification

- [x] 3.1 Run `make` and confirm vet, the full suite and the build are green, `TestEveryDecisionNamesTheEvent` included.
- [x] 3.2 With a prepared AVD, issue one real `mcp__mobile-mcp__mobile_click_on_screen_at_coordinates` call from a session served by the pool and confirm it reaches the session's instance instead of failing the tool's own schema validation.
