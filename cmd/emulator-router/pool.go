package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// entry — запись реестра. Имя файла это консольный порт инстанса, как в
// bash-версии, поэтому порт в теле не дублируется. JSON вместо строчного
// формата: там cwd с пробелом сдвигал разбор остальных полей.
type entry struct {
	Session    string `json:"session"`
	OwnerPID   int    `json:"owner_pid"`
	OwnerStart string `json:"owner_start"`
	CWD        string `json:"cwd"`
	Since      int64  `json:"since"`
}

var (
	digits = regexp.MustCompile(`^[0-9]+$`)
	spaces = regexp.MustCompile(` +`)
)

func entryPath(ctx context.Context, port int) string {
	return filepath.Join(cfg(ctx).Registry, strconv.Itoa(port))
}
func logPath(ctx context.Context, port int) string { return entryPath(ctx, port) + ".log" }

func entries(ctx context.Context) []int {
	c := cfg(ctx)
	names, err := os.ReadDir(c.Registry)
	if err != nil {
		return nil
	}
	var ports []int
	for _, n := range names {
		if digits.MatchString(n.Name()) {
			p, _ := strconv.Atoi(n.Name())
			ports = append(ports, p)
		}
	}
	return ports
}

func readEntry(ctx context.Context, port int) (entry, bool) {
	var e entry
	b, err := os.ReadFile(entryPath(ctx, port))
	if err != nil || json.Unmarshal(b, &e) != nil {
		return e, false
	}
	return e, true
}

func writeEntry(ctx context.Context, port int, e entry) error {
	c := cfg(ctx)
	if err := os.MkdirAll(c.Registry, 0o755); err != nil {
		return err
	}
	b, err := json.Marshal(e)
	if err != nil {
		return err
	}
	return os.WriteFile(entryPath(ctx, port), b, 0o644)
}

func dropEntry(ctx context.Context, port int) {
	os.Remove(entryPath(ctx, port))
	os.Remove(logPath(ctx, port))
}

// procStart — время запуска процесса. Номер процесса сам по себе не
// идентификатор: система его переиспользует.
func procStart(ctx context.Context, pid int) string {
	c := cfg(ctx)
	out, err := c.Run("ps", "-o", "lstart=", "-p", strconv.Itoa(pid))
	if err != nil {
		return ""
	}
	return spaces.ReplaceAllString(strings.ReplaceAll(out, "\n", ""), "_")
}

func ownerAlive(ctx context.Context, e entry) bool {
	c := cfg(ctx)
	if e.OwnerPID <= 0 {
		return true // владелец не определён: судить о его смерти не по чему
	}
	if c.Signal(e.OwnerPID, 0) != nil {
		return false
	}
	return e.OwnerStart == "" || e.OwnerStart == procStart(ctx, e.OwnerPID)
}

// deviceState: присутствие в перечислении это не готовность. После перезапуска
// сервера отладки инстанс десятки секунд значится offline.
func deviceState(ctx context.Context, port int) string {
	c := cfg(ctx)
	out, err := c.Run(adb(ctx), "devices")
	if err != nil {
		return ""
	}
	want := serial(port)
	for _, line := range strings.Split(out, "\n") {
		f := strings.Fields(line)
		if len(f) >= 2 && f[0] == want {
			return f[1]
		}
	}
	return ""
}

func serial(port int) string { return "emulator-" + strconv.Itoa(port) }

func qemuPID(ctx context.Context, port int) (int, bool) {
	c := cfg(ctx)
	out, err := c.Run("pgrep", "-f", fmt.Sprintf("qemu-system.*-port %d( |$)", port))
	if err != nil {
		return 0, false
	}
	for _, line := range strings.Split(out, "\n") {
		if pid, err := strconv.Atoi(strings.TrimSpace(line)); err == nil && pid > 0 {
			return pid, true
		}
	}
	return 0, false
}

func killInstance(ctx context.Context, port int) {
	c := cfg(ctx)
	c.Run(adb(ctx), "-s", serial(port), "emu", "kill")
	if pid, ok := qemuPID(ctx, port); ok {
		c.Sleep(2 * time.Second)
		if c.Signal(pid, 0) == nil {
			c.Signal(pid, syscall.SIGKILL)
		}
	}
}

// reap убирает три вида мусора: записи с мёртвым владельцем, записи с живым
// владельцем и погибшим инстансом, неиспользуемые COW-слои.
//
// Инстансы, отсутствующие в собственном реестре, не трогаются вовсе: bash-версия
// прибивала все read-only процессы эмулятора на машине, спецификация это запрещает.
func reap(ctx context.Context) {
	c := cfg(ctx)
	for _, port := range entries(ctx) {
		e, ok := readEntry(ctx, port)
		if !ok {
			continue
		}
		if ownerAlive(ctx, e) {
			// Запись создаётся до запуска, поэтому свежую запись трогать нельзя:
			// это завершило бы ещё загружающийся инстанс.
			if c.Now().Unix()-e.Since <= int64(c.BootTimeout.Seconds()) {
				continue
			}
			if _, live := qemuPID(ctx, port); live {
				continue
			}
		}
		killInstance(ctx, port)
		dropEntry(ctx, port)
	}
	// При принудительном завершении qemu не удаляет свой слой сам.
	layers, _ := filepath.Glob(filepath.Join(c.COWDir, "emulator-*.qcow2"))
	for _, f := range layers {
		if _, err := c.Run("lsof", "-t", f); err != nil {
			os.Remove(f)
		}
	}
}

// mine ищет инстанс сессии или её владельца. wait включает ожидание
// восстановления временно недоступного инстанса.
func mine(ctx context.Context, session string, owner int, wait bool) (int, bool) {
	c := cfg(ctx)
	for _, port := range entries(ctx) {
		e, ok := readEntry(ctx, port)
		if !ok {
			continue
		}
		if e.Session != session && !(owner != 0 && e.OwnerPID == owner) {
			continue
		}
		state := deviceState(ctx, port)
		// Недоступность при живом процессе это почти всегда перезапуск сервера
		// отладки: инстанс возвращается сам, и утилизация стоила бы сессии
		// полной загрузки нового.
		if state != "device" && wait {
			if _, live := qemuPID(ctx, port); live {
				c.Run(adb(ctx), "reconnect", "offline")
				for waited := time.Duration(0); waited < c.OfflineTimeout && state != "device"; waited += 2 * time.Second {
					c.Sleep(2 * time.Second)
					state = deviceState(ctx, port)
				}
			}
		}
		// Запись переживает свой инстанс при аварии процесса или закрытии окна;
		// выдача такого серийника привязала бы сессию к мёртвому устройству.
		if state != "device" {
			killInstance(ctx, port)
			dropEntry(ctx, port)
			continue
		}
		return port, true
	}
	return 0, false
}

func portFree(ctx context.Context, port int) bool {
	c := cfg(ctx)
	if _, err := os.Stat(entryPath(ctx, port)); err == nil {
		return false
	}
	if out, err := c.Run(adb(ctx), "devices"); err == nil && strings.Contains(out, serial(port)) {
		return false
	}
	l, err := net.Listen("tcp", ":"+strconv.Itoa(port))
	if err != nil {
		return false
	}
	l.Close()
	return true
}

// lock сериализует запуски между процессами: одновременный старт двух инстансов
// ломает загрузку из снапшота. flock, а не каталог-лок: ядро снимает блокировку
// при завершении процесса любым способом, поэтому «пул заклинен навсегда»
// недостижимо.
func lock(ctx context.Context) (*os.File, error) {
	c := cfg(ctx)
	if err := os.MkdirAll(c.Registry, 0o755); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(filepath.Join(c.Registry, ".launch.lock"), os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return nil, err
	}
	for waited := time.Duration(0); ; waited += 2 * time.Second {
		if syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB) == nil {
			return f, nil
		}
		if waited > c.LockTimeout {
			f.Close()
			return nil, fmt.Errorf("another acquire holds the launch lock")
		}
		c.Sleep(2 * time.Second)
	}
}

func unlock(f *os.File) {
	syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
	f.Close()
}

// waitBoot: загруженный это ещё не пригодный. Пока свежий клон регистрируется в
// сервере отладки, обращения к любому инстансу могут не выполняться, поэтому
// готовностью считаются два последовательных подтверждения.
func waitBoot(ctx context.Context, port int) bool {
	c := cfg(ctx)
	streak := 0
	for waited := time.Duration(0); waited < c.BootTimeout; waited += 3 * time.Second {
		out, err := c.Run(adb(ctx), "-s", serial(port), "shell", "getprop", "sys.boot_completed")
		if err == nil && strings.TrimSpace(strings.ReplaceAll(out, "\r", "")) == "1" {
			if streak++; streak >= 2 {
				return true
			}
		} else {
			streak = 0
		}
		c.Sleep(3 * time.Second)
	}
	return false
}

func acquire(ctx context.Context, session string, owner int, cwd string) (string, error) {
	c := cfg(ctx)
	if session == "" {
		return "", fmt.Errorf("session id required")
	}
	if err := os.MkdirAll(c.Registry, 0o755); err != nil {
		return "", err
	}
	reap(ctx)

	if port, ok := mine(ctx, session, owner, true); ok {
		return serial(port), nil
	}

	if !avdExists(ctx) {
		return "", fmt.Errorf("AVD %q not found, prepare it first", c.AVD)
	}

	// Блокировка держится сквозь всю загрузку намеренно.
	lk, err := lock(ctx)
	if err != nil {
		return "", err
	}
	defer unlock(lk)

	if n := len(entries(ctx)); n >= c.Max {
		return "", fmt.Errorf("all %d instances are busy\n%s", c.Max, statusText(ctx))
	}

	port := 0
	for p := c.PortMin; p <= c.PortMax; p += 2 {
		if portFree(ctx, p) {
			port = p
			break
		}
	}
	if port == 0 {
		return "", fmt.Errorf("no free port in %d-%d", c.PortMin, c.PortMax)
	}

	// Запись создаётся до запуска, чтобы порт не был выдан дважды.
	if err := writeEntry(ctx, port, entry{
		Session:    session,
		OwnerPID:   owner,
		OwnerStart: procStart(ctx, owner),
		CWD:        cwd,
		Since:      c.Now().Unix(),
	}); err != nil {
		return "", err
	}

	if err := c.Spawn(logPath(ctx, port), emu(ctx), "@"+c.AVD, "-read-only",
		"-port", strconv.Itoa(port), "-gpu", c.GPU, "-no-window", "-no-audio", "-no-boot-anim"); err != nil {
		dropEntry(ctx, port)
		return "", err
	}

	if !waitBoot(ctx, port) {
		killInstance(ctx, port)
		dropEntry(ctx, port)
		return "", fmt.Errorf("instance on port %d did not boot in %s, see %s",
			port, c.BootTimeout, logPath(ctx, port))
	}
	return serial(port), nil
}

func avdExists(ctx context.Context) bool {
	c := cfg(ctx)
	out, err := c.Run(emu(ctx), "-list-avds")
	if err != nil {
		return false
	}
	for _, line := range strings.Split(out, "\n") {
		if strings.TrimSpace(line) == c.AVD {
			return true
		}
	}
	return false
}

func release(ctx context.Context, session string, owner int) error {
	port, ok := mine(ctx, session, owner, false)
	if !ok {
		return nil // освобождать нечего это успех
	}
	killInstance(ctx, port)
	dropEntry(ctx, port)
	return nil
}

func statusText(ctx context.Context) string {
	var b strings.Builder
	for _, port := range entries(ctx) {
		e, ok := readEntry(ctx, port)
		if !ok {
			continue
		}
		state := "dead"
		if ownerAlive(ctx, e) {
			state = "alive"
		}
		fmt.Fprintf(&b, "%s %s %d %s\n", serial(port), state, e.OwnerPID, e.CWD)
	}
	return b.String()
}
