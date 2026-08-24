## 1. Introduce the effect

- [x] 1.1 Add the named seam type, its context key and the `run(ctx, name, args...)` helper to `main.go`, mirroring `cfgKey`/`cfg`; rename the current production implementation so the short name is free for the helper, and place the effect into the context in `main` next to the config. Verify `make build` succeeds while `config.Run` is still present and still wired.
- [x] 1.2 Build the test context from an explicit runner: `ctxOf` takes it as a second argument and puts it under the effect key — the fake's runner for the fast suite, the production one for the `emulators`-tagged tests, which have no fake. Test helpers that only received a config (`runHook`, `unreadableEntry`) take it too. Verify `make test` stays green — at this point both wirings exist, so nothing has moved yet.

## 2. Move the call sites

- [x] 2.1 Replace all 22 `c.Run(...)` calls in `pool.go`, `doctor.go` and `hook.go` with `run(ctx, ...)`, drop the `c := cfg(ctx)` this leaves unused (`procStart`, `deviceState`, `qemuPID`, `portFree`, `ownerPID`, `liveChecks`), and hand the effect to the five tests that used to intercept the seam by overwriting `c.Run` after the config was built. Verify `make test` is green and `go vet` reports no unused variables.
- [x] 2.2 Remove the `Run` field from `config`, its default wiring in `defaultConfig`, and its assignment in `newTestConfig`; update the comment above the remaining seams so it names four functions, not five. Verify the package compiles with no leftover references and `make test` is green.

## 3. Verification

- [x] 3.1 Run `make` and confirm vet, the full suite and the build are all green.
- [x] 3.2 Reviewed the resulting diff and decided the trial stops here: `Run` is the only seam whose travel inside the config dragged a settings dependency through unrelated functions (six of them lost it). `Spawn` has one call site, `Signal`'s four all sit in functions that read settings anyway, and `Now`/`Sleep` are everywhere next to settings reads — moving those would be a rearrangement with no payoff. The remaining four seams stay fields of `config`; no follow-up change is opened.
