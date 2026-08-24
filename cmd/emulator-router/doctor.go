// Проверка предусловий: отделяет сломанное окружение от сломанного кода. Сначала
// дешёвые статические проверки, потом один настоящий инстанс — то, что нельзя
// установить статически.
package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

type level int

const (
	pass level = iota
	warn
	fail
)

func (l level) String() string {
	switch l {
	case warn:
		return "warn"
	case fail:
		return "FAIL"
	}
	return "ok  "
}

// finding — результат одной проверки. Сообщается по отдельности: итог не говорит,
// что именно сломано, а именно это и требуется от проверки предусловий.
type finding struct {
	level  level
	detail string
}

type report []finding

func (r *report) add(l level, format string, args ...any) {
	*r = append(*r, finding{l, fmt.Sprintf(format, args...)})
}

func (r report) failed() bool {
	for _, f := range r {
		if f.level == fail {
			return true
		}
	}
	return false
}

// doctor: код возврата определяется только нарушениями. Предупреждение означает
// «пул работает, но хуже», поэтому на код не влияет.
func doctor(ctx context.Context, cwd string, stdout io.Writer) int {
	c, r := staticChecks(ctx, cwd)

	if r.failed() {
		// Загрузка инстанса стоит минут, а в заведомо сломанном окружении не
		// говорит ничего нового.
		r.add(warn, "skipping live checks: fix the failures above first")
	} else {
		r = append(r, liveChecks(cfgKey.With(ctx, c))...)
	}

	for _, f := range r {
		fmt.Fprintf(stdout, "%s  %s\n", f.level, f.detail)
	}
	if r.failed() {
		return 1
	}
	return 0
}

// staticChecks возвращает также применённые настройки: часть проверок зависит от
// образа, а образ приходит из файла настроек проекта.
func staticChecks(ctx context.Context, cwd string) (config, report) {
	c := cfg(ctx)
	var r report

	// Первой строкой и до любого досрочного возврата: проверка отвечает на вопрос
	// «почему пул ведёт себя не так», и ответом бывает устаревший бинарь по
	// прежнему пути. Пустая величина приходит из сборки без git, поэтому
	// отсутствие версии называется явно, а не печатается пустым местом.
	v := version
	if v == "" {
		v = "unknown"
	}
	r.add(pass, "version: %s", v)

	for _, tool := range []struct{ name, path string }{
		{"adb", adb(ctx)},
		{"emulator", emu(ctx)},
		{"qemu-img", qemuImg(ctx)},
	} {
		if executable(tool.path) {
			r.add(pass, "%s: %s", tool.name, tool.path)
		} else {
			r.add(fail, "%s is not executable at %s (set ANDROID_HOME or ANDROID_SDK_ROOT)", tool.name, tool.path)
		}
	}

	// Настройки проверяются здесь, потому что при обработке события хука их
	// отсутствие по спецификации не даёт никакого вывода: это единственное место,
	// где «проект не настроен» может быть названо громко.
	path, _ := findSettings(cwd)
	if path == "" {
		r.add(fail, "no %s found above %s: the hook stays silent without it", settingsFile, cwd)
		return c, r
	}
	applied, err := applySettings(c, path)
	if err != nil {
		r.add(fail, "%v", err)
		return c, r
	}
	c = applied
	r.add(pass, "settings: %s (avd %q, max %d, gpu %q, %d configured wrappers)",
		path, c.AVD, c.Max, c.GPU, len(c.Reach))

	r = append(r, wiringCheck(cwd)...)

	ctx = cfgKey.With(ctx, c)
	if avdExists(ctx) {
		r.add(pass, "avd %q exists", c.AVD)
	} else {
		r.add(fail, "avd %q not found among available images", c.AVD)
	}

	r = append(r, snapshotChecks(ctx)...)
	r = append(r, diskCheck(ctx)...)
	return c, r
}

// agentSettings — файлы настроек агента, в которых может быть подключён хук.
// Локальные и общие настройки проекта плюс общие настройки пользователя: строка
// хука живёт в одном из них, и какой именно — свойство установки, а не наше дело.
var agentSettings = []string{
	filepath.Join(".claude", "settings.json"),
	filepath.Join(".claude", "settings.local.json"),
}

// wiringCheck: вторая половина молчаливого отказа. Первую — ненайденный файл
// настроек — ловит проверка выше; здесь настройки есть, а вызывать роутер некому,
// и от исправной работы это состояние неотличимо ничем: все остальные проверки,
// включая запуск настоящего инстанса, проходят успешно.
func wiringCheck(cwd string) report {
	var r report
	for dir := cwd; ; {
		for _, name := range agentSettings {
			path := filepath.Join(dir, name)
			if wiredIn(path) {
				r.add(pass, "hook wired in %s", path)
				return r
			}
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}
		dir = parent
	}
	home, _ := os.UserHomeDir()
	user := filepath.Join(home, ".claude", "settings.json")
	if wiredIn(user) {
		r.add(pass, "hook wired in %s", user)
		return r
	}
	r.add(fail, "no %s hook found in %s or %s: the pool is never called, and every other check still passes",
		"emulator-router", strings.Join(agentSettings, ", "), user)
	return r
}

// wiredIn ищет именно вызов подкоманды hook, а не упоминание имени программы:
// иначе проверка подтверждала бы подключение, которого нет.
func wiredIn(path string) bool {
	b, err := os.ReadFile(path)
	if err != nil {
		return false
	}
	for line := range strings.SplitSeq(string(b), "\n") {
		if _, after, ok := strings.Cut(line, "emulator-router"); ok {
			if rest := strings.TrimSpace(after); strings.HasPrefix(rest, "hook") {
				return true
			}
		}
	}
	return false
}

func qemuImg(ctx context.Context) string {
	return filepath.Join(cfg(ctx).SDK, "emulator", "qemu-img")
}

func executable(path string) bool {
	fi, err := os.Stat(path)
	return err == nil && !fi.IsDir() && fi.Mode()&0o111 != 0
}

// baseImages — образы, внутри которых живёт состояние диска на момент снятия
// снапшота. Перечисление, а не обход каталога: отсутствие снапшота в одном из
// этих образов и есть нарушение, а лишние файлы рядом к делу не относятся.
var baseImages = []string{"userdata-qemu.img.qcow2", "cache.img.qcow2", "encryptionkey.img.qcow2"}

// snapshotChecks: снапшот оперативной памяти — только половина дела. Состояние
// диска лежит внутри базовых образов, и запуск образа в режиме записи его
// уничтожает, чего ram.bin не показывает.
func snapshotChecks(ctx context.Context) report {
	c := cfg(ctx)
	var r report
	avdDir := filepath.Join(avdHome(), c.AVD+".avd")
	snap := filepath.Join(avdDir, "snapshots", "default_boot")

	if _, err := os.Stat(filepath.Join(snap, "ram.bin")); err == nil {
		r.add(pass, "snapshot default_boot: %s", snap)
	} else {
		r.add(fail, "no snapshot default_boot at %s: instances would cold boot instead of loading it", snap)
		return r
	}

	var missing []string
	for _, img := range baseImages {
		out, err := run(ctx, qemuImg(ctx), "snapshot", "-l", "-U", filepath.Join(avdDir, img))
		if err != nil || !strings.Contains(out, "default_boot") {
			missing = append(missing, img)
		}
	}
	if len(missing) == 0 {
		r.add(pass, "snapshot default_boot present in all base images")
	} else {
		r.add(fail, "no default_boot inside %s: clones cold boot, regenerate the snapshot from the main checkout",
			strings.Join(missing, ", "))
	}

	cfgIni, _ := os.ReadFile(filepath.Join(avdDir, "config.ini"))
	if hasLine(cfgIni, "fastboot.forceColdBoot=no") && hasLine(cfgIni, "fastboot.forceFastBoot=yes") {
		r.add(pass, "avd config asks for quick boot")
	} else {
		r.add(warn, "avd config does not force quick boot: check fastboot.* in %s", filepath.Join(avdDir, "config.ini"))
	}

	// Ускоритель, записанный в снапшот, должен совпадать с тем, что получает
	// безоконный инстанс: иначе загрузка снапшота отваливается и каждый инстанс
	// молча выполняет полную загрузку.
	if gpu := iniValue(filepath.Join(snap, "hardware.ini"), "hw.gpu.mode"); gpu != "" {
		if gpu == c.GPU {
			r.add(pass, "snapshot renderer: %s", gpu)
		} else {
			r.add(warn, "snapshot renderer %q differs from configured gpu %q: the snapshot may fail to load", gpu, c.GPU)
		}
	} else {
		r.add(warn, "cannot read renderer from %s", filepath.Join(snap, "hardware.ini"))
	}
	return r
}

func avdHome() string {
	if v := os.Getenv("ANDROID_AVD_HOME"); v != "" {
		return v
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".android", "avd")
}

func hasLine(content []byte, want string) bool {
	for line := range strings.SplitSeq(string(content), "\n") {
		if strings.TrimSpace(line) == want {
			return true
		}
	}
	return false
}

func iniValue(path, key string) string {
	b, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	for line := range strings.SplitSeq(string(b), "\n") {
		k, v, ok := strings.Cut(line, "=")
		if ok && strings.TrimSpace(k) == key {
			return strings.TrimSpace(v)
		}
	}
	return ""
}

// diskCheck: нехватка места — предупреждение. Пул с меньшим числом одновременных
// инстансов работоспособен, поэтому на код возврата это не влияет; оценка нужна
// затем, чтобы объяснить, почему очередная сессия инстанса не получила.
func diskCheck(ctx context.Context) report {
	c := cfg(ctx)
	var r report
	need := c.Max * c.DiskPerInstanceGB
	free, err := freeGB(ctx, c.COWDir)
	if err != nil {
		r.add(warn, "cannot measure free disk space: %v", err)
		return r
	}
	if free >= need {
		r.add(pass, "disk free: %dG, enough for %d instances at %dG each", free, c.Max, c.DiskPerInstanceGB)
	} else {
		r.add(warn, "disk free: %dG, but %d instances need %dG: the pool will run out before the cap", free, c.Max, need)
	}
	return r
}

// freeGB спрашивает у df, а не у syscall: df уже проходит через тот же шов, что
// остальной внешний мир, поэтому подменяется в тестах наравне с ним.
func freeGB(ctx context.Context, path string) (int, error) {
	// Слои копирования при записи могут лежать не на том разделе, что корень,
	// поэтому спрашиваем про их каталог, а не про "/". Каталога может ещё не быть.
	for path != "" {
		if _, err := os.Stat(path); err == nil {
			break
		}
		parent := filepath.Dir(path)
		if parent == path {
			break
		}
		path = parent
	}
	out, err := run(ctx, "df", "-k", path)
	if err != nil {
		return 0, err
	}
	lines := strings.Split(strings.TrimSpace(out), "\n")
	if len(lines) < 2 {
		return 0, fmt.Errorf("unexpected df output: %q", out)
	}
	f := strings.Fields(lines[len(lines)-1])
	if len(f) < 4 {
		return 0, fmt.Errorf("unexpected df output: %q", out)
	}
	kb, err := strconv.ParseInt(f[3], 10, 64)
	if err != nil {
		return 0, fmt.Errorf("unexpected df output: %q", out)
	}
	return int(kb / 1024 / 1024), nil
}

// liveChecks проверяет то, что статически неустановимо: загрузилась ли машина из
// снапшота, не работает ли демон отладки с правами суперпользователя (снапшот,
// снятый в этом режиме, делает непригодными все клоны) и доступны ли права
// суперпользователя для отдельной команды.
func liveChecks(ctx context.Context) report {
	var r report
	session := "doctor-" + strconv.Itoa(os.Getpid())

	// Владелец — сам процесс проверки: запись с неопределённым владельцем
	// считается принадлежащей живой сессии, и уборка не забирает её никогда, так
	// что прерванная проверка держала бы место в пределе до ручной чистки.
	owner := os.Getpid()
	serial, err := acquire(ctx, session, owner, ".")
	if err != nil {
		r.add(fail, "acquire failed: %v", err)
		return r
	}
	// Освобождение при любом исходе, включая нарушение: иначе doctor в сломанном
	// окружении сам занимал бы место в пределе.
	defer release(ctx, session, owner)
	r.add(pass, "acquire works: %s", serial)

	port, _ := strconv.Atoi(strings.TrimPrefix(serial, "emulator-"))
	log, _ := os.ReadFile(logPath(ctx, port))
	if strings.Contains(string(log), "Successfully loaded snapshot") {
		r.add(pass, "instance loaded the snapshot")
	} else {
		r.add(fail, "instance did not load the snapshot, it cold booted: see %s", logPath(ctx, port))
	}

	switch out, err := run(ctx, adb(ctx), "-s", serial, "shell", "id"); {
	case err != nil:
		r.add(fail, "cannot read the adb identity on %s: %v", serial, err)
	case strings.Contains(out, "uid=0"):
		r.add(fail, "adbd runs as root on %s: clones fight over one adb identity, re-save the snapshot", serial)
	case strings.Contains(out, "uid="):
		r.add(pass, "adbd is not rooted on %s", serial)
	default:
		r.add(fail, "unexpected adb identity on %s: %q", serial, strings.TrimSpace(out))
	}

	if out, err := run(ctx, adb(ctx), "-s", serial, "shell", "su", "0", "id"); err == nil && strings.Contains(out, "uid=0") {
		r.add(pass, "root for a single command works")
	} else {
		r.add(fail, "'su 0' does not give root on %s: scenarios that need it will fail", serial)
	}
	return r
}
