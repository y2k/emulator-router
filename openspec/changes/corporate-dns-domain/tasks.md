# Tasks

## 1. Refactor: isolate launch shape and instance identity (no behavior change)

- [ ] 1.1 Extract `launchArgs(ctx, port) []string` from the `c.Spawn(...)` call in `acquire` (`cmd/emulator-router/pool.go`); verify `go test ./...` passes with no test edits.
- [ ] 1.2 Extract `foreign(ctx, e entry) string` from `mine`: returns the kill reason (`image-mismatch was=... want=...`) or `""`; `mine` kills and drops on a non-empty reason. Verify `TestInstanceOfAnotherImageIsNotHandedBack`, `TestEntryWithoutImageIsNotHandedBack` and the rest of `go test ./...` pass with no test edits.

## 2. Settings field `dns`

- [ ] 2.1 Add `DNS *string \`json:"dns"\`` to `settings` and `DNSDomain` to `config` in `cmd/emulator-router/main.go`; in `applySettings` normalize (lowercase, strip trailing dot) and reject an empty string or a value accepted by `net.ParseIP`, with an error naming the field and saying the server comes from the machine's resolver configuration. Extend `TestSettingsDefaults` (no domain), `TestSettingsOverrideDefaults` (`"Corp.Example."` → `corp.example`) and `TestSettingsRejectBadFiles` (IP, empty string); verify they pass.

## 3. `dns.go`: resolver lookup

- [ ] 3.1 Create `cmd/emulator-router/dns.go` with the `scutil --dns` parser and `serverFor(domain, scutilOutput) string` (design decision 3). Add a table test using the real `scutil --dns` output from the spike machine as fixture, internal zones and addresses replaced with placeholders (the repo is public): exact zone, parent zone, most specific zone wins, unrelated zone → `""`, blocks without domain or nameserver skipped, the `for scoped queries` section ignored. Verify the test passes.
- [ ] 3.2 Add `withHostDNS(ctx) context.Context` to `dns.go`: with an empty `DNSDomain` it returns `ctx` untouched and runs nothing; otherwise it runs `scutil --dns` through the `run` seam and stores the result in `config.DNSServer`, an error yielding `""`. Verify with tests: no `scutil` call without a domain (the fake runner fails on unexpected calls), server set when the fake returns the fixture, `""` when the fake returns an error.

## 4. Pool wiring

- [ ] 4.1 Add `DNS string \`json:"dns,omitempty"\`` to `entry`; call `ctx = withHostDNS(ctx)` once at the start of `acquire` (the single switch, design decision 2); write `c.DNSServer` into the new entry; append `-dns-server <server>` in `launchArgs` when set; add `dns=<server>` before `cwd` in the launch journal line when set. Verify with tests: with a domain and the fixture the spawn args contain `-dns-server 10.0.0.53`, the entry records it and the journal line names it; without a domain the spawn args and journal line are unchanged.
- [ ] 4.2 In `foreign`, after the image check, compare `e.DNS` with `c.DNSServer` and return `dns-mismatch was=<server|none> want=<server|none>`. Verify with tests: an entry with another server is killed with that reason and the session gets a new instance; an entry without `dns` in a project without the domain is handed back unchanged.

## 5. Doctor

- [ ] 5.1 Add `dnsCheck(ctx) report` to `dns.go` and append it in `staticChecks` after the settings line: absent domain → no line; resolver found → `pass` naming domain and server; not found → `warn` naming the domain and suggesting VPN. Verify with three tests in `doctor_test.go`, including that the warning keeps exit code 0.

## 6. Documentation

- [ ] 6.1 Add the `dns` row to the settings table in `README.md` and a short paragraph: a domain, not an address; the server comes from the machine's resolvers; macOS only; toggling VPN relaunches the instance. Verify the example settings JSON in README is still valid for `applySettings`.
- [ ] 6.2 Describe `dns` in `skills/emulator-router-setup/SKILL.md` next to `reach`/`mainCheckout`, and add the symptom "names of the corporate zone do not resolve on the device" with the `dns` field and `doctor` to `skills/emulator-router-usage/SKILL.md`. Verify the skills consistency test passes.
- [ ] 6.3 Update `CLAUDE.md`: architecture lists five files with `dns.go`; the settings section lists the current settings including the corporate DNS domain. Verify by reading the sections against the code.

## 7. Integration

- [ ] 7.1 Run `make` and verify vet, tests and build pass.
- [ ] 7.2 On this machine with VPN on: in a temporary directory with `.claude/emulator-router.json` = `{"avd": "agent_base", "dns": "corp.example"}`, run `bin/emulator-router acquire <session> <pid> <dir>`, then verify `adb -s <serial> shell ping -c1 s3.stage.corp.example` resolves to `10.0.1.10` without any hosts edit, `emulator-router log` shows `dns=10.0.0.53`, and the instance log shows the snapshot loaded; then `release` it.

## Workflow follow-up

- Ask the user before adding `"dns": "corp.example"` to the work app's `.claude/emulator-router.json` and before removing the temporary hosts bind-mount on `emulator-5556`.
- Archive the change after review; spec deltas sync into the main specs.
