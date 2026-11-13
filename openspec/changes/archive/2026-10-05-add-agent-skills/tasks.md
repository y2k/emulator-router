## 1. Consistency test

- [x] 1.1 Extract the `dispatch()` case-label collection from `TestHelpNamesEverySubcommand` in `cmd/emulator-router/router_test.go` into a helper returning the labels, and switch that test to it. Verify with `go test ./cmd/emulator-router -run TestHelpNamesEverySubcommand -v`.
- [x] 1.2 Add a test that reads `../../skills/*/SKILL.md`, fails when no skill is found, requires the frontmatter `name:` to equal the directory name, and requires every `emulator-router <word>` inside inline code or code blocks to be a `dispatch()` label. Verify it fails when a skill mentions `emulator-router nosuch` in code or carries a mismatched `name:`, and passes once tasks 2.x are done.

## 2. Skills

- [x] 2.1 Write `skills/emulator-router-setup/SKILL.md` in English: a description naming the connect-and-repair scenario and pointing to `emulator-router-usage` for device work; steps from `go install` through hook wiring (both events, explicit timeout above the router budget, with why), `.claude/emulator-router.json` (`avd`, `reach`, `mainCheckout`; the cap and renderer must not be set), AVD preparation with a boot snapshot and `-read-only` launches only, to `doctor` exiting 0 as the only completion criterion. Point to `emulator-router help` instead of listing subcommands or exit codes. Verify against the `agent-skills` spec requirements for scenarios, setup, self-containment and consistency.
- [x] 2.2 Write `skills/emulator-router-usage/SKILL.md` in English: a description naming device work in a connected project and hook denials, pointing to `emulator-router-setup` for setup; never pick a device or pass a serial by hand; the idle sweep takes the instance and its state, so reinstall after a gap and confirm with `emulator-router log`; a wrapper missing from `reach` bypasses the pool silently; the response to each denial (retry on "retry in a moment", never bypass and tell the user on "all N instances are busy", `adb shell su 0 <command>` instead of `adb root`, `-read-only` for the AVD); `emulator-router status` for occupancy. Verify against the `agent-skills` spec requirements for scenarios, usage, denials, self-containment and consistency.
- [x] 2.3 Check that neither skill references a path outside its own directory. Verify by grepping both files for relative links and `../`.

## 3. Documentation

- [x] 3.1 Add a "Skills" section to `README.md`: what each skill is for and how to link a skill directory into a project's `.claude/skills/` (`ln -s`), keeping the link local (`.git/info/exclude`) or copying instead, since an absolute link breaks for a second contributor. Verify by reading it against design.md risks.
- [x] 3.2 Run `make` and verify vet, tests, and build all pass.
