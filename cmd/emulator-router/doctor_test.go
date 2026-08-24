package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// doctorStand собирает окружение исправным: исполнимые заглушки утилит SDK,
// каталог образа со снапшотом и файл настроек проекта. Каждый тест ломает ровно
// одно место, чтобы видеть, что нарушение называется именно оно.
type doctorStand struct {
	cwd    string
	sdk    string
	avdDir string
	c      config
	f      *fake
}

func newDoctorStand(t *testing.T) *doctorStand {
	t.Helper()
	c, f := newTestConfig(t)

	sdk := t.TempDir()
	for _, rel := range []string{"platform-tools/adb", "emulator/emulator", "emulator/qemu-img"} {
		p := filepath.Join(sdk, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte("#!/bin/sh\n"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	c.SDK = sdk

	avdHome := t.TempDir()
	t.Setenv("ANDROID_AVD_HOME", avdHome)
	avdDir := filepath.Join(avdHome, "pooled_avd.avd")
	snap := filepath.Join(avdDir, "snapshots", "default_boot")
	if err := os.MkdirAll(snap, 0o755); err != nil {
		t.Fatal(err)
	}
	write := func(path, body string) {
		if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write(filepath.Join(snap, "ram.bin"), "x")
	write(filepath.Join(snap, "hardware.ini"), "hw.gpu.mode = host\n")
	write(filepath.Join(avdDir, "config.ini"), "fastboot.forceColdBoot=no\nfastboot.forceFastBoot=yes\n")

	t.Setenv("HOME", t.TempDir())

	cwd := worktreeDir(t)
	writeAgentSettings(t, cwd, "settings.json", `{"hooks": {"PreToolUse": [{"command": "/opt/bin/emulator-router hook"}]}}`)
	f.snapshots = map[string]bool{}
	for _, img := range baseImages {
		f.snapshots[filepath.Join(avdDir, img)] = true
	}
	f.freeKB = 100 * 1024 * 1024 // 100G

	return &doctorStand{cwd: cwd, sdk: sdk, avdDir: avdDir, c: c, f: f}
}

// writeAgentSettings кладёт настройки агента (не роутера) в каталог dir.
func writeAgentSettings(t *testing.T, dir, name, body string) string {
	t.Helper()
	path := filepath.Join(dir, ".claude", name)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func (s *doctorStand) run(t *testing.T) (int, string) {
	t.Helper()
	var out bytes.Buffer
	code := doctor(ctxOf(s.c), s.cwd, &out)
	return code, out.String()
}

// Исправное окружение: ни одного нарушения, живые проверки выполняются.
func TestDoctorPassesInHealthyEnvironment(t *testing.T) {
	s := newDoctorStand(t)
	code, out := s.run(t)
	if code != 0 {
		t.Fatalf("код=%d, ожидался 0\n%s", code, out)
	}
	if strings.Contains(out, "FAIL") {
		t.Fatalf("нарушений быть не должно:\n%s", out)
	}
	for _, want := range []string{
		"adb:", "emulator:", "settings:", `avd "pooled_avd" exists`,
		"snapshot default_boot", "present in all base images", "disk free",
		"acquire works", "loaded the snapshot", "adbd is not rooted", "root for a single command works",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("в выводе нет проверки %q:\n%s", want, out)
		}
	}
}

// Каждая проверка сообщается по отдельности, а не только итог.
func TestDoctorReportsEachCheckSeparately(t *testing.T) {
	s := newDoctorStand(t)
	_, out := s.run(t)
	lines := strings.Split(strings.TrimSpace(out), "\n")
	if len(lines) < 8 {
		t.Fatalf("ожидался отчёт по каждой проверке, получено %d строк:\n%s", len(lines), out)
	}
	for _, line := range lines {
		if !strings.HasPrefix(line, "ok") && !strings.HasPrefix(line, "warn") && !strings.HasPrefix(line, "FAIL") {
			t.Errorf("строка без уровня: %q", line)
		}
	}
}

func TestDoctorFailsOnMissingTool(t *testing.T) {
	s := newDoctorStand(t)
	if err := os.Remove(filepath.Join(s.sdk, "platform-tools", "adb")); err != nil {
		t.Fatal(err)
	}
	code, out := s.run(t)
	if code == 0 {
		t.Fatalf("недоступный инструмент это нарушение:\n%s", out)
	}
	if !strings.Contains(out, "ANDROID_HOME") || !strings.Contains(out, s.sdk) {
		t.Errorf("причина должна называть ожидаемое расположение и способ его задать:\n%s", out)
	}
}

func TestDoctorFailsOnMissingAVD(t *testing.T) {
	s := newDoctorStand(t)
	s.f.avds = []string{"other_avd"}
	code, out := s.run(t)
	if code == 0 || !strings.Contains(out, `avd "pooled_avd" not found`) {
		t.Fatalf("отсутствие образа это нарушение:\n%s", out)
	}
}

// Отсутствие настроек при обработке хука по спецификации молчит, поэтому здесь
// оно обязано быть громким.
func TestDoctorFailsWithoutSettings(t *testing.T) {
	s := newDoctorStand(t)
	s.cwd = t.TempDir()
	code, out := s.run(t)
	if code == 0 {
		t.Fatalf("ненайденные настройки это нарушение:\n%s", out)
	}
	if !strings.Contains(out, "emulator-router.json") || !strings.Contains(out, "silent") {
		t.Errorf("причина должна называть файл и последствие:\n%s", out)
	}
}

func TestDoctorFailsOnBadSettings(t *testing.T) {
	s := newDoctorStand(t)
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, ".git"), []byte("gitdir: /elsewhere\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	writeSettings(t, dir, `{"avd": "img", "typo": 1}`)
	s.cwd = dir
	code, out := s.run(t)
	if code == 0 || !strings.Contains(out, "typo") {
		t.Fatalf("негодные настройки это нарушение с причиной:\n%s", out)
	}
}

func TestDoctorNamesAppliedSettingsFile(t *testing.T) {
	s := newDoctorStand(t)
	_, out := s.run(t)
	want := filepath.Join(s.cwd, settingsFile)
	if !strings.Contains(out, want) {
		t.Errorf("вывод должен называть применённый файл %q:\n%s", want, out)
	}
}

func TestDoctorFailsOnMissingSnapshot(t *testing.T) {
	s := newDoctorStand(t)
	if err := os.Remove(filepath.Join(s.avdDir, "snapshots", "default_boot", "ram.bin")); err != nil {
		t.Fatal(err)
	}
	code, out := s.run(t)
	if code == 0 || !strings.Contains(out, "cold boot") {
		t.Fatalf("отсутствие снапшота это нарушение с последствием:\n%s", out)
	}
}

// Снапшот оперативной памяти — половина дела: состояние диска живёт внутри
// базовых образов, и его уничтожает запуск образа в режиме записи.
func TestDoctorFailsOnSnapshotMissingFromBaseImage(t *testing.T) {
	s := newDoctorStand(t)
	s.f.snapshots[filepath.Join(s.avdDir, "cache.img.qcow2")] = false
	code, out := s.run(t)
	if code == 0 || !strings.Contains(out, "cache.img.qcow2") {
		t.Fatalf("отсутствие снапшота в базовом образе это нарушение:\n%s", out)
	}
}

func TestDoctorWarnsOnQuickBootNotForced(t *testing.T) {
	s := newDoctorStand(t)
	if err := os.WriteFile(filepath.Join(s.avdDir, "config.ini"), []byte("fastboot.forceColdBoot=yes\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	code, out := s.run(t)
	if code != 0 {
		t.Fatalf("предупреждение не должно влиять на код возврата:\n%s", out)
	}
	if !strings.Contains(out, "warn") || !strings.Contains(out, "quick boot") {
		t.Errorf("ожидалось предупреждение о быстрой загрузке:\n%s", out)
	}
}

func TestDoctorWarnsOnInsufficientDisk(t *testing.T) {
	s := newDoctorStand(t)
	s.f.freeKB = 1024 * 1024 // 1G на предел из трёх инстансов
	code, out := s.run(t)
	if code != 0 {
		t.Fatalf("нехватка места это предупреждение, а не нарушение:\n%s", out)
	}
	if !strings.Contains(out, "warn") || !strings.Contains(out, "run out before the cap") {
		t.Errorf("ожидалось предупреждение о месте:\n%s", out)
	}
}

// Запускать инстанс в заведомо сломанном окружении бессмысленно, а его загрузка
// стоит минут.
func TestDoctorSkipsLiveChecksAfterFailure(t *testing.T) {
	s := newDoctorStand(t)
	s.f.avds = []string{"other_avd"}
	_, out := s.run(t)
	if !strings.Contains(out, "skipping live checks") {
		t.Errorf("живые проверки должны быть пропущены:\n%s", out)
	}
	if strings.Contains(out, "acquire works") {
		t.Errorf("инстанс не должен запускаться:\n%s", out)
	}
	if len(s.f.spawns) != 0 {
		t.Errorf("инстанс не должен запускаться: %v", s.f.spawns)
	}
}

func TestDoctorFailsOnColdBoot(t *testing.T) {
	s := newDoctorStand(t)
	s.f.bootLog = "emulator: WARNING: could not load snapshot, performing a full boot"
	code, out := s.run(t)
	if code == 0 || !strings.Contains(out, "cold booted") {
		t.Fatalf("полная загрузка вместо снапшота это нарушение:\n%s", out)
	}
}

func TestDoctorFailsOnRootedAdbd(t *testing.T) {
	s := newDoctorStand(t)
	s.f.shellID = "uid=0(root) gid=0(root)"
	code, out := s.run(t)
	if code == 0 || !strings.Contains(out, "adbd runs as root") {
		t.Fatalf("рутованный демон отладки это нарушение:\n%s", out)
	}
}

func TestDoctorFailsWhenRootForSingleCommandIsUnavailable(t *testing.T) {
	s := newDoctorStand(t)
	s.f.suWorks = false
	code, out := s.run(t)
	if code == 0 || !strings.Contains(out, "su 0") {
		t.Fatalf("недоступность прав суперпользователя это нарушение:\n%s", out)
	}
}

// Иначе doctor в сломанном окружении сам занимал бы место в пределе.
func TestDoctorReleasesProbeInstanceOnFailure(t *testing.T) {
	for _, tc := range []struct {
		name   string
		break_ func(*doctorStand)
	}{
		{"полная загрузка", func(s *doctorStand) { s.f.bootLog = "full boot" }},
		{"рутованный демон", func(s *doctorStand) { s.f.shellID = "uid=0(root)" }},
		{"нет прав суперпользователя", func(s *doctorStand) { s.f.suWorks = false }},
	} {
		s := newDoctorStand(t)
		tc.break_(s)
		if code, out := s.run(t); code == 0 {
			t.Fatalf("%s: ожидалось нарушение:\n%s", tc.name, out)
		}
		if left := entries(ctxOf(s.c)); len(left) != 0 {
			t.Errorf("%s: проверочный инстанс не освобождён: %v", tc.name, left)
		}
	}
}

// Проверочная запись принадлежит процессу проверки: запись с неопределённым
// владельцем считается принадлежащей живой сессии, и уборка не забирает её
// никогда — прерванная проверка занимала бы место в пределе до ручной чистки.
func TestDoctorProbeEntryIsOwnedByCheckingProcess(t *testing.T) {
	s := newDoctorStand(t)
	// Запись существует только между выдачей и освобождением, поэтому снимается
	// изнутри проверки на запущенном инстансе.
	inner := s.c.Run
	var probe entry
	s.c.Run = func(name string, args ...string) (string, error) {
		if len(args) == 4 && args[0] == "-s" && args[2] == "shell" && args[3] == "id" {
			for _, port := range entries(ctxOf(s.c)) {
				probe, _ = readEntry(ctxOf(s.c), port)
			}
		}
		return inner(name, args...)
	}
	if code, out := s.run(t); code != 0 {
		t.Fatalf("код=%d, ожидался 0\n%s", code, out)
	}
	if probe.OwnerPID != os.Getpid() {
		t.Fatalf("владелец проверочной записи %d, ожидался процесс проверки %d", probe.OwnerPID, os.Getpid())
	}
	s.f.dead[probe.OwnerPID] = true
	if ownerAlive(ctxOf(s.c), probe) {
		t.Error("прерванная проверка должна оставлять запись, собираемую обычной уборкой")
	}
}

// Вторая половина молчаливого отказа: настройки роутера есть, а вызывать его
// некому. От исправной работы это неотличимо ничем — все остальные проверки,
// включая запуск настоящего инстанса, проходят.
func TestDoctorFailsWithoutHookWiring(t *testing.T) {
	s := newDoctorStand(t)
	if err := os.Remove(filepath.Join(s.cwd, ".claude", "settings.json")); err != nil {
		t.Fatal(err)
	}
	code, out := s.run(t)
	if code == 0 {
		t.Fatalf("неподключённый хук это нарушение:\n%s", out)
	}
	if !strings.Contains(out, "the pool is never called") {
		t.Errorf("причина должна называть последствие:\n%s", out)
	}
	if strings.Contains(out, "acquire works") {
		t.Errorf("живые проверки должны быть пропущены:\n%s", out)
	}
}

func TestDoctorFindsHookWiringAtEachLevel(t *testing.T) {
	wiring := `{"hooks": {"PreToolUse": [{"command": "/opt/bin/emulator-router hook"}]}}`
	cases := []struct {
		name  string
		place func(t *testing.T, s *doctorStand) string
	}{
		{"общие настройки проекта", func(t *testing.T, s *doctorStand) string {
			return writeAgentSettings(t, s.cwd, "settings.json", wiring)
		}},
		{"локальные настройки проекта", func(t *testing.T, s *doctorStand) string {
			return writeAgentSettings(t, s.cwd, "settings.local.json", wiring)
		}},
		{"настройки выше рабочего каталога", func(t *testing.T, s *doctorStand) string {
			// Углубляем рабочий каталог, а не пишем в родителя: родитель у всех
			// временных каталогов теста общий, и запись туда утекала бы в соседние
			// случаи.
			path := writeAgentSettings(t, s.cwd, "settings.json", wiring)
			sub := filepath.Join(s.cwd, "app", "src")
			if err := os.MkdirAll(sub, 0o755); err != nil {
				t.Fatal(err)
			}
			s.cwd = sub
			return path
		}},
		{"общие настройки агента", func(t *testing.T, s *doctorStand) string {
			home := t.TempDir()
			t.Setenv("HOME", home)
			return writeAgentSettings(t, home, "settings.json", wiring)
		}},
	}
	for _, tc := range cases {
		s := newDoctorStand(t)
		if err := os.Remove(filepath.Join(s.cwd, ".claude", "settings.json")); err != nil {
			t.Fatal(err)
		}
		want := tc.place(t, s)
		code, out := s.run(t)
		if code != 0 {
			t.Errorf("%s: подключение не найдено:\n%s", tc.name, out)
			continue
		}
		if !strings.Contains(out, want) {
			t.Errorf("%s: вывод должен называть файл %q:\n%s", tc.name, want, out)
		}
	}
}

// Упоминание имени программы подключением не является: иначе проверка
// подтверждала бы подключение, которого нет.
func TestDoctorDoesNotMistakeMentionForWiring(t *testing.T) {
	s := newDoctorStand(t)
	writeAgentSettings(t, s.cwd, "settings.json",
		`{"permissions": {"allow": ["Bash(/opt/bin/emulator-router status)"]}}`)
	code, out := s.run(t)
	if code == 0 || !strings.Contains(out, "the pool is never called") {
		t.Fatalf("упоминание имени программы это не подключение хука:\n%s", out)
	}
}
