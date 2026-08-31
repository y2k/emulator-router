package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// fake подменяет весь внешний мир: тесты не запускают эмуляторы, не трогают
// настоящие утилиты SDK и не обращаются к живым реестрам.
type fake struct {
	devices map[int]string
	qemu    map[int]int
	avds    []string
	ppid    map[int]int
	comm    map[int]string
	starts  map[int]string
	dead    map[int]bool
	open    map[string]bool
	// noLsof — средства проверки занятости в системе нет: команда не запускается
	// вовсе, а не отвечает «никем не открыт».
	noLsof bool
	spawns []int
	now    int64

	// Для doctor: снапшоты в базовых образах, свободное место, лог загрузки
	// инстанса, идентичность демона отладки и доступность прав суперпользователя.
	snapshots map[string]bool
	freeKB    int64
	bootLog   string
	logPath   string
	shellID   string
	suWorks   bool
}

// ctxOf собирает контекст из настроек и швов: эффект запуска команды едет
// отдельным ключом, поэтому подставить его в config больше нельзя. Аргумент —
// сам эффект, а не fake: тестам с настоящими инстансами нужен настоящий запуск.
func ctxOf(c config, r runner) context.Context {
	return runKey.With(cfgKey.With(context.Background(), c), r)
}

func newTestConfig(t *testing.T) (config, *fake) {
	t.Helper()
	f := &fake{
		devices: map[int]string{}, qemu: map[int]int{},
		avds: []string{"pooled_avd"},
		ppid: map[int]int{}, comm: map[int]string{}, starts: map[int]string{},
		dead: map[int]bool{}, open: map[string]bool{},
		now:       1_700_000_000,
		snapshots: map[string]bool{},
		freeKB:    100 * 1024 * 1024,
		bootLog:   "emulator: Successfully loaded snapshot 'default_boot'",
		shellID:   "uid=2000(shell) gid=2000(shell)",
		suWorks:   true,
	}
	c := defaultConfig()
	c.AVD = "pooled_avd"
	c.SDK = "/sdk"
	c.Registry = t.TempDir()
	c.COWDir = t.TempDir()
	c.PortMin, c.PortMax = 15556, 15566
	c.Spawn = func(logPath, name string, args ...string) error {
		f.logPath = logPath
		return f.spawn(args)
	}
	c.Signal = func(pid int, sig syscall.Signal) error {
		if f.dead[pid] {
			return syscall.ESRCH
		}
		return nil
	}
	c.Now = func() time.Time { return time.Unix(f.now, 0) }
	// Сон не тратит настоящего времени, но двигает часы: пределы выдачи заданы
	// сроками, и без движения часов срок не наступал бы никогда.
	c.Sleep = func(d time.Duration) { f.now += int64(d.Seconds()) }
	return c, f
}

func (f *fake) spawn(args []string) error {
	port := 0
	for i, a := range args {
		if a == "-port" && i+1 < len(args) {
			port, _ = strconv.Atoi(args[i+1])
		}
	}
	f.spawns = append(f.spawns, port)
	f.devices[port] = "device"
	f.qemu[port] = 90000 + port
	// doctor читает лог инстанса, чтобы отличить загрузку из снапшота от полной.
	if f.logPath != "" {
		if err := os.WriteFile(f.logPath, []byte(f.bootLog+"\n"), 0o644); err != nil {
			return err
		}
	}
	return nil
}

func (f *fake) run(name string, args ...string) (string, error) {
	// Путь сверяется хвостом, а не целиком: заглушки утилит SDK у doctor лежат в
	// собственном временном каталоге, а не в "/sdk".
	switch {
	case strings.HasSuffix(name, "/platform-tools/adb"):
		return f.adb(args)
	case strings.HasSuffix(name, "/emulator/emulator"):
		if len(args) == 1 && args[0] == "-list-avds" {
			return strings.Join(f.avds, "\n") + "\n", nil
		}
		return "", errors.New("unexpected emulator call: " + strings.Join(args, " "))
	case strings.HasSuffix(name, "/emulator/qemu-img"):
		if len(args) == 4 && args[0] == "snapshot" {
			if f.snapshots[args[3]] {
				return "Snapshot list:\nID  TAG           VM SIZE\n1   default_boot    1.0G\n", nil
			}
			return "Snapshot list:\n", nil
		}
		return "", errors.New("unexpected qemu-img call")
	}
	switch name {
	case "df":
		return fmt.Sprintf("Filesystem 1024-blocks Used Available Capacity\n/dev/disk1 100 1 %d 1%%\n", f.freeKB), nil
	case "ps":
		return f.ps(args)
	case "pgrep":
		return f.pgrep(args)
	case "lsof":
		if f.noLsof {
			return "", &exec.Error{Name: "lsof", Err: exec.ErrNotFound}
		}
		if len(args) == 2 && args[0] == "-t" {
			if f.open[args[1]] {
				return "1234\n", nil
			}
			return "", errors.New("not open")
		}
	}
	return "", errors.New("unexpected call: " + name + " " + strings.Join(args, " "))
}

func (f *fake) adb(args []string) (string, error) {
	if len(args) == 0 {
		return "", errors.New("bad adb")
	}
	switch args[0] {
	case "devices":
		ports := make([]int, 0, len(f.devices))
		for p := range f.devices {
			ports = append(ports, p)
		}
		sort.Ints(ports)
		b := strings.Builder{}
		b.WriteString("List of devices attached\n")
		for _, p := range ports {
			fmt.Fprintf(&b, "emulator-%d\t%s\n", p, f.devices[p])
		}
		return b.String(), nil
	case "reconnect":
		return "", nil
	case "-s":
		port, _ := strconv.Atoi(strings.TrimPrefix(args[1], "emulator-"))
		switch {
		case len(args) >= 4 && args[2] == "emu" && args[3] == "kill":
			delete(f.devices, port)
			delete(f.qemu, port)
			return "", nil
		case len(args) >= 4 && args[2] == "shell" && args[3] == "getprop":
			if f.devices[port] == "device" {
				return "1\r\n", nil
			}
			return "", errors.New("device offline")
		case len(args) == 4 && args[2] == "shell" && args[3] == "id":
			return f.shellID + "\n", nil
		case len(args) == 6 && args[2] == "shell" && args[3] == "su" && args[4] == "0" && args[5] == "id":
			if !f.suWorks {
				return "", errors.New("su: permission denied")
			}
			return "uid=0(root) gid=0(root)\n", nil
		}
	}
	return "", errors.New("bad adb: " + strings.Join(args, " "))
}

func (f *fake) ps(args []string) (string, error) {
	if len(args) != 4 {
		return "", errors.New("bad ps")
	}
	pid, _ := strconv.Atoi(args[3])
	switch args[1] {
	case "ppid=":
		p, ok := f.ppid[pid]
		if !ok {
			return "", errors.New("no such process")
		}
		return fmt.Sprintf("  %d\n", p), nil
	case "comm=":
		return f.comm[pid] + "\n", nil
	case "lstart=":
		s, ok := f.starts[pid]
		if !ok {
			return "", errors.New("no such process")
		}
		return s + "\n", nil
	}
	return "", errors.New("bad ps")
}

func (f *fake) pgrep(args []string) (string, error) {
	if len(args) != 2 || args[0] != "-f" {
		return "", errors.New("bad pgrep")
	}
	for port, pid := range f.qemu {
		if strings.Contains(args[1], fmt.Sprintf("-port %d(", port)) {
			return strconv.Itoa(pid) + "\n", nil
		}
	}
	return "", errors.New("no match")
}

// --- вспомогательное ---

// worktreeDir: связанное рабочее дерево со своим файлом настроек. Дерево без
// собственного файла, берущее настройки из главного чекаута, — отдельный случай
// (TestWorktreeTakesSettingsFromRepository).
func worktreeDir(t *testing.T) string {
	t.Helper()
	d := t.TempDir()
	if err := os.WriteFile(filepath.Join(d, ".git"), []byte("gitdir: /elsewhere\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	writeSettings(t, d, `{"avd": "pooled_avd"}`)
	return d
}

// Настройки принадлежат репозиторию: связанное рабочее дерево без собственного
// файла берёт их из главного чекаута. Иначе каждое новое дерево молча оставалось
// бы без пула — а именно деревьям пул и нужен.
func TestWorktreeTakesSettingsFromRepository(t *testing.T) {
	repo := t.TempDir()
	gitdir := filepath.Join(repo, ".git")
	wtGitdir := filepath.Join(gitdir, "worktrees", "feature")
	if err := os.MkdirAll(wtGitdir, 0o755); err != nil {
		t.Fatal(err)
	}
	want := writeSettings(t, repo, `{"avd": "pooled_avd"}`)

	// Дерево лежит вне чекаута: обход вверх до главного чекаута не дошёл бы.
	wt := filepath.Join(t.TempDir(), "feature")
	if err := os.MkdirAll(wt, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(wt, ".git"), []byte("gitdir: "+wtGitdir+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	path, mainCheckout := findSettings(wt)
	if path != want {
		t.Errorf("настройки репозитория: получено %q, ожидалось %q", path, want)
	}
	if mainCheckout {
		t.Error("связанное рабочее дерево не является главным чекаутом")
	}

	// Собственный файл дерева побеждает файл репозитория.
	own := writeSettings(t, wt, `{"avd": "own_avd"}`)
	if path, _ := findSettings(wt); path != own {
		t.Errorf("побеждает собственный файл дерева: получено %q, ожидалось %q", path, own)
	}
}

// Границу репозитория обход не переходит: настройки постороннего родительского
// каталога в проект не утекают.
func TestSettingsDoNotLeakAcrossRepositoryBoundary(t *testing.T) {
	outer := t.TempDir()
	writeSettings(t, outer, `{"avd": "outer_avd"}`)
	inner := filepath.Join(outer, "vendored")
	if err := os.MkdirAll(filepath.Join(inner, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	if path, _ := findSettings(inner); path != "" {
		t.Errorf("настройки не должны браться из чужого репозитория: %q", path)
	}
}

func mainCheckoutDir(t *testing.T) string {
	t.Helper()
	d := t.TempDir()
	if err := os.Mkdir(filepath.Join(d, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	writeSettings(t, d, `{"avd": "pooled_avd"}`)
	return d
}

// writeSettings кладёт файл настроек проекта в каталог dir.
func writeSettings(t *testing.T, dir, body string) string {
	t.Helper()
	path := filepath.Join(dir, settingsFile)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func hookJSON(t *testing.T, event, session, cwd, tool string, toolInput map[string]any) string {
	t.Helper()
	m := map[string]any{"hook_event_name": event, "session_id": session, "cwd": cwd, "tool_name": tool}
	if toolInput != nil {
		m["tool_input"] = toolInput
	}
	b, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func bashJSON(t *testing.T, session, cwd, cmd string) string {
	t.Helper()
	return hookJSON(t, "PreToolUse", session, cwd, "Bash", map[string]any{"command": cmd})
}

func runHook(t *testing.T, c config, f *fake, input string) (int, string, string) {
	t.Helper()
	var out, errOut bytes.Buffer
	code := hook(ctxOf(c, f.run), strings.NewReader(input), &out, &errOut)
	return code, out.String(), errOut.String()
}

func decode(t *testing.T, out string) hookSpecific {
	t.Helper()
	var o hookOut
	if err := json.Unmarshal([]byte(out), &o); err != nil {
		t.Fatalf("вывод не разбирается как JSON: %q: %v", out, err)
	}
	return o.HookSpecificOutput
}

// seed кладёт готовый инстанс в реестр, чтобы выдача не требовала загрузки.
func seed(t *testing.T, c config, f *fake, port int, e entry) {
	t.Helper()
	if e.Since == 0 {
		e.Since = f.now
	}
	// Готовый инстанс принадлежит работающей сессии: без отметки он был бы записью
	// полувекового простоя. Тесты уборки по простою задают LastSeen сами.
	if e.LastSeen == 0 {
		e.LastSeen = f.now
	}
	if e.Image == "" {
		e.Image = c.AVD // инстанс пула поднят из образа этих настроек
	}
	if err := writeEntry(ctxOf(c, f.run), port, e); err != nil {
		t.Fatal(err)
	}
	f.devices[port] = "device"
	f.qemu[port] = 90000 + port
}

// --- гейт по рабочему каталогу ---

func TestFindSettingsDetectsMainCheckout(t *testing.T) {
	main := mainCheckoutDir(t)
	sub := filepath.Join(main, "app", "src")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	outside := t.TempDir()
	writeSettings(t, outside, `{"avd": "pooled_avd"}`)
	cases := []struct {
		name string
		dir  string
		want bool
	}{
		{"главный чекаут", main, true},
		{"подкаталог главного чекаута", sub, true},
		{"связанное рабочее дерево", worktreeDir(t), false},
		{"каталог вне репозитория", outside, false},
	}
	for _, tc := range cases {
		path, mainCheckout := findSettings(tc.dir)
		if path == "" {
			t.Errorf("%s: настройки не найдены", tc.name)
		}
		if mainCheckout != tc.want {
			t.Errorf("%s: mainCheckout = %v, ожидалось %v", tc.name, mainCheckout, tc.want)
		}
	}
}

func TestFindSettingsWalksUp(t *testing.T) {
	root := t.TempDir()
	deep := filepath.Join(root, "a", "b")
	if err := os.MkdirAll(deep, 0o755); err != nil {
		t.Fatal(err)
	}

	if path, _ := findSettings(deep); path != "" {
		t.Fatalf("файла нет, а найден %q", path)
	}

	up := writeSettings(t, root, `{"avd": "outer"}`)
	if path, _ := findSettings(deep); path != up {
		t.Errorf("настройки выше рабочего каталога: получено %q, ожидалось %q", path, up)
	}

	near := writeSettings(t, deep, `{"avd": "inner"}`)
	path, _ := findSettings(deep)
	if path != near {
		t.Errorf("побеждает ближний уровень: получено %q, ожидалось %q", path, near)
	}
	c, err := applySettings(defaultConfig(), path)
	if err != nil {
		t.Fatal(err)
	}
	if c.AVD != "inner" {
		t.Errorf("значения не сливаются между уровнями: avd=%q, ожидался inner", c.AVD)
	}
}

func TestSettingsDefaults(t *testing.T) {
	dir := t.TempDir()
	path := writeSettings(t, dir, `{"avd": "img"}`)
	c, err := applySettings(defaultConfig(), path)
	if err != nil {
		t.Fatal(err)
	}
	if c.AVD != "img" || c.Max != 3 || c.GPU != "host" || c.MainCheckout || len(c.Reach) != 0 {
		t.Errorf("дефолты не применились: %+v", struct {
			AVD          string
			Max          int
			GPU          string
			MainCheckout bool
			Reach        int
		}{c.AVD, c.Max, c.GPU, c.MainCheckout, len(c.Reach)})
	}
}

func TestSettingsOverrideDefaults(t *testing.T) {
	dir := t.TempDir()
	path := writeSettings(t, dir, `{"avd": "img", "mainCheckout": true, "reach": ["wrapper\\.sh"]}`)
	c, err := applySettings(defaultConfig(), path)
	if err != nil {
		t.Fatal(err)
	}
	if !c.MainCheckout || len(c.Reach) != 1 {
		t.Errorf("настройки не применились: %+v", c)
	}
	// Предел и ускоритель настройками не переопределяются вовсе.
	if c.Max != 3 || c.GPU != "host" {
		t.Errorf("машинные значения изменились: max %d, gpu %q", c.Max, c.GPU)
	}
}

func TestSettingsRejectBadFiles(t *testing.T) {
	cases := []struct{ name, body, want string }{
		{"неразбираемое содержимое", `{не json`, "invalid character"},
		{"нет обязательного поля", `{}`, `"avd" is required`},
		{"незнакомое поле", `{"avd": "img", "maxx": 2}`, "maxx"},
		{"неподходящий тип", `{"avd": 5}`, "avd"},
		{"негодная регулярка", `{"avd": "img", "reach": ["([unclosed"]}`, "([unclosed"},
		// Предел и ускоритель настройками проекта не являются, поэтому заданными
		// делают файл негодным наравне с опечаткой в имени поля.
		{"задан предел инстансов", `{"avd": "img", "max": 3}`, "max"},
		{"задан ускоритель графики", `{"avd": "img", "gpu": "host"}`, "gpu"},
		{"задан предел простоя", `{"avd": "img", "idleTimeout": 300}`, "idleTimeout"},
	}
	for _, tc := range cases {
		path := writeSettings(t, t.TempDir(), tc.body)
		_, err := applySettings(defaultConfig(), path)
		if err == nil {
			t.Errorf("%s: ошибка не возвращена", tc.name)
			continue
		}
		if !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: причина %q не называет %q", tc.name, err, tc.want)
		}
	}
}

func TestSDKPathFromEnvironment(t *testing.T) {
	t.Setenv("ANDROID_HOME", "")
	t.Setenv("ANDROID_SDK_ROOT", "/sdk-root")
	if got := sdkPath("/home"); got != "/sdk-root" {
		t.Errorf("ANDROID_SDK_ROOT не учтён: %q", got)
	}
	t.Setenv("ANDROID_HOME", "/sdk-home")
	if got := sdkPath("/home"); got != "/sdk-home" {
		t.Errorf("ANDROID_HOME должен побеждать: %q", got)
	}
	t.Setenv("ANDROID_HOME", "")
	t.Setenv("ANDROID_SDK_ROOT", "")
	want := "/home/Android/Sdk"
	if runtime.GOOS == "darwin" {
		want = "/home/Library/Android/sdk"
	}
	if got := sdkPath("/home"); got != want {
		t.Errorf("платформенное расположение: получено %q, ожидалось %q", got, want)
	}
}

// Запрошенная справка — это результат работы, а не сообщение о неверном вызове:
// иначе вызывающий скрипт видит отказ там, где отказа нет.
func TestHelpIsSuccess(t *testing.T) {
	for _, arg := range []string{"help", "--help", "-h"} {
		c, f := newTestConfig(t)
		var out, errOut bytes.Buffer
		code := dispatch(ctxOf(c, f.run), []string{arg}, nil, &out, &errOut)
		if code != 0 {
			t.Errorf("%s: код=%d, ожидался 0 (stderr=%q)", arg, code, errOut.String())
		}
		if errOut.Len() != 0 {
			t.Errorf("%s: справка не должна попадать в поток ошибок: %q", arg, errOut.String())
		}
		if !strings.Contains(out.String(), "usage:") {
			t.Errorf("%s: справка должна быть на stdout: %q", arg, out.String())
		}
	}
}

// Вызов без аргументов — это status, а не справка: значение вызова без
// аргументов изменение справки не затрагивает.
func TestNoArgumentsRunsStatus(t *testing.T) {
	c, f := newTestConfig(t)
	seed(t, c, f, 15556, entry{Session: "s1", CWD: t.TempDir()})
	var out, errOut bytes.Buffer
	if code := dispatch(ctxOf(c, f.run), nil, nil, &out, &errOut); code != 0 {
		t.Fatalf("код=%d stderr=%q", code, errOut.String())
	}
	if !strings.Contains(out.String(), "emulator-15556") {
		t.Errorf("ожидался вывод status: %q", out.String())
	}
	if strings.Contains(out.String(), "usage:") {
		t.Errorf("справка выводиться не должна: %q", out.String())
	}
}

// Неизвестная подкоманда остаётся ошибкой и не выглядит как успешный вызов:
// вместо экрана справки — имя нераспознанного и указатель, как справку получить.
func TestUnknownSubcommandIsError(t *testing.T) {
	c, f := newTestConfig(t)
	var out, errOut bytes.Buffer
	code := dispatch(ctxOf(c, f.run), []string{"nosuch"}, nil, &out, &errOut)
	if code == 0 {
		t.Fatalf("ожидался ненулевой код, получено %d", code)
	}
	if out.Len() != 0 {
		t.Errorf("ошибочный вызов не должен ничего печатать на stdout: %q", out.String())
	}
	msg := errOut.String()
	if !strings.Contains(msg, "nosuch") {
		t.Errorf("сообщение должно называть нераспознанное имя: %q", msg)
	}
	if !strings.Contains(msg, "--help") {
		t.Errorf("сообщение должно указывать способ получить справку: %q", msg)
	}
	if strings.Contains(msg, "usage:") {
		t.Errorf("сообщение не должно быть самой справкой: %q", msg)
	}
}

// Подкоманда, отсутствующая в справке, для того, у кого нет доступа к
// репозиторию, не существует, поэтому полноту перечня закрепляет тест, а не
// аккуратность автора: метки берутся из самого разбора аргументов.
//
// ponytail: тест знает устройство dispatch() — единственный switch по строковым
// меткам. При отказе от switch переписывается вместе с ним; таблица подкоманд в
// проде дала бы гарантию по построению, но обошлась бы дороже самой справки.
func TestHelpNamesEverySubcommand(t *testing.T) {
	src, err := parser.ParseFile(token.NewFileSet(), "main.go", nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	var labels []string
	for _, decl := range src.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if !ok || fn.Name.Name != "dispatch" {
			continue
		}
		ast.Inspect(fn, func(n ast.Node) bool {
			cl, ok := n.(*ast.CaseClause)
			if !ok {
				return true
			}
			for _, e := range cl.List {
				if lit, ok := e.(*ast.BasicLit); ok && lit.Kind == token.STRING {
					labels = append(labels, strings.Trim(lit.Value, `"`))
				}
			}
			return true
		})
	}
	if len(labels) == 0 {
		t.Fatal("в dispatch() не найдено ни одной метки: разбор аргументов перестроен, тест надо переписать")
	}

	// Сверяется перечень, а не весь текст: имя подкоманды попадается и в
	// поясняющих абзацах, и поиск по всему тексту пропустил бы выпавшую строку.
	// Строка перечня — отступ, имена через запятую, два пробела, описание; за
	// именем могут стоять его аргументы, поэтому берётся первое слово.
	listed := map[string]bool{}
	for line := range strings.SplitSeq(helpText, "\n") {
		if !strings.HasPrefix(line, "  ") {
			continue
		}
		names, _, _ := strings.Cut(strings.TrimSpace(line), "  ")
		for part := range strings.SplitSeq(names, ", ") {
			if fields := strings.Fields(part); len(fields) > 0 {
				listed[fields[0]] = true
			}
		}
	}
	for _, name := range labels {
		if !listed[name] {
			t.Errorf("подкоманда %q не названа в перечне справки", name)
		}
	}
}

// Подкоманды реестра образа не знают, поэтому файла настроек не требуют: иначе
// осмотреть и убрать пул было бы нельзя, не находясь в настроенном проекте.
func TestRegistrySubcommandsWorkWithoutSettings(t *testing.T) {
	c, f := newTestConfig(t)
	c.AVD = "" // как в дефолтах: образ приходит только из файла настроек
	seed(t, c, f, 15556, entry{Session: "s1", CWD: t.TempDir()})
	var out, errOut bytes.Buffer

	if code := dispatch(ctxOf(c, f.run), []string{"status"}, nil, &out, &errOut); code != 0 {
		t.Fatalf("status: код=%d stderr=%q", code, errOut.String())
	}
	if !strings.Contains(out.String(), "emulator-15556") {
		t.Errorf("status должен показывать занятый инстанс: %q", out.String())
	}
	if code := dispatch(ctxOf(c, f.run), []string{"reap"}, nil, &out, &errOut); code != 0 {
		t.Fatalf("reap: код=%d stderr=%q", code, errOut.String())
	}
}

// Подкоманду acquire запускают руками и именно для этого каталога, поэтому здесь
// ненайденный файл — ошибка, а не молчание.
func TestAcquireSubcommandRequiresSettings(t *testing.T) {
	c, f := newTestConfig(t)
	var out, errOut bytes.Buffer
	code := dispatch(ctxOf(c, f.run), []string{"acquire", "s1", "0", t.TempDir()}, nil, &out, &errOut)
	if code != 1 {
		t.Fatalf("ожидался код 1, получено %d (out=%q)", code, out.String())
	}
	if !strings.Contains(errOut.String(), "emulator-router.json") {
		t.Errorf("причина должна называть файл настроек: %q", errOut.String())
	}
}

// Ненайденный файл настроек — «проект не мой»: хук подключают общими настройками
// агента, поэтому блокировать вызов в непричастном проекте нельзя.
func TestUnconfiguredProjectIsNotServed(t *testing.T) {
	c, f := newTestConfig(t)
	dir := t.TempDir()
	code, out, errOut := runHook(t, c, f, bashJSON(t, "s1", dir, "adb install app.apk"))
	if code != 0 || out != "" {
		t.Fatalf("ожидалось молчание, получено код=%d out=%q err=%q", code, out, errOut)
	}
	if len(f.spawns) != 0 {
		t.Fatalf("инстанс не должен запускаться: %v", f.spawns)
	}
}

// Найденный файл означает сознательную настройку: продолжать на дефолтах нельзя.
// Пределы времени настройками проекта не являются, поэтому заданный предел
// простоя блокирует вызов наравне с опечаткой в имени поля.
func TestBadSettingsBlockTheCall(t *testing.T) {
	for _, tc := range []struct{ name, body, want string }{
		{"незнакомое поле", `{"avd": "img", "typo": 1}`, "typo"},
		{"задан предел простоя", `{"avd": "img", "idleTimeout": 300}`, "idleTimeout"},
	} {
		c, f := newTestConfig(t)
		dir := t.TempDir()
		if err := os.WriteFile(filepath.Join(dir, ".git"), []byte("gitdir: /elsewhere\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		writeSettings(t, dir, tc.body)
		code, out, errOut := runHook(t, c, f, bashJSON(t, "s1", dir, "adb install app.apk"))
		if code != 2 {
			t.Fatalf("%s: ожидался код 2, получено %d (out=%q)", tc.name, code, out)
		}
		if !strings.Contains(errOut, tc.want) {
			t.Errorf("%s: причина должна называть поле: %q", tc.name, errOut)
		}
		if len(f.spawns) != 0 {
			t.Fatalf("%s: инстанс не должен запускаться: %v", tc.name, f.spawns)
		}
	}
}

// Освобождение не зависит от настроек: реестр один на машину, и образ для
// освобождения не нужен.
func TestSessionEndReleasesWithoutSettings(t *testing.T) {
	for _, tc := range []struct{ name, body string }{
		{"файла нет", ""},
		{"файл негоден", `{"typo": 1}`},
	} {
		c, f := newTestConfig(t)
		dir := t.TempDir()
		if tc.body != "" {
			writeSettings(t, dir, tc.body)
		}
		seed(t, c, f, 15556, entry{Session: "s1", CWD: dir})
		code, _, errOut := runHook(t, c, f, hookJSON(t, "SessionEnd", "s1", dir, "", nil))
		if code != 0 {
			t.Fatalf("%s: код=%d stderr=%q", tc.name, code, errOut)
		}
		if _, err := os.Stat(entryPath(ctxOf(c, f.run), 15556)); !os.IsNotExist(err) {
			t.Errorf("%s: запись должна быть удалена", tc.name)
		}
	}
}

// Проект без связанных рабочих деревьев иначе не получает пул вовсе.
func TestMainCheckoutServedWhenConfigured(t *testing.T) {
	c, f := newTestConfig(t)
	main := t.TempDir()
	if err := os.Mkdir(filepath.Join(main, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	writeSettings(t, main, `{"avd": "pooled_avd", "mainCheckout": true}`)
	seed(t, c, f, 15556, entry{Session: "s1", CWD: main})
	code, out, errOut := runHook(t, c, f, bashJSON(t, "s1", main, "adb install app.apk"))
	if code != 0 || out == "" {
		t.Fatalf("главный чекаут должен обслуживаться: код=%d out=%q err=%q", code, out, errOut)
	}
}

// Обёртки проекта распознаются, но требование позиции команды навешивает роутер,
// а не автор шаблона: иначе неосторожный шаблон снимал бы гарантию молча.
func TestConfiguredReachKeepsCommandPosition(t *testing.T) {
	cwd := t.TempDir()
	if err := os.WriteFile(filepath.Join(cwd, ".git"), []byte("gitdir: /elsewhere\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	writeSettings(t, cwd, `{"avd": "pooled_avd", "reach": ["wrapper\\.sh", "tools[^ ]*\\.kts"]}`)
	cases := []struct {
		cmd  string
		want bool
	}{
		{"./wrapper.sh install", true},
		{"cd app && ./scripts/wrapper.sh", true},
		{"kotlin tools.main.kts status", true},
		{`echo "run wrapper.sh later"`, false},
		{"grep -r wrapper.sh .", false},
		{"./other.sh", false},
	}
	for _, tc := range cases {
		c, f := newTestConfig(t)
		seed(t, c, f, 15556, entry{Session: "s1", CWD: cwd})
		_, out, _ := runHook(t, c, f, bashJSON(t, "s1", cwd, tc.cmd))
		if got := out != ""; got != tc.want {
			t.Errorf("%q: распознано=%v, ожидалось %v (out=%q)", tc.cmd, got, tc.want, out)
		}
	}
}

func TestMainCheckoutNotTouched(t *testing.T) {
	c, f := newTestConfig(t)
	code, out, _ := runHook(t, c, f, bashJSON(t, "s1", mainCheckoutDir(t), "adb install app.apk"))
	if code != 0 || out != "" {
		t.Fatalf("в главном чекауте ожидалось молчание, получено код=%d out=%q", code, out)
	}
	if len(f.spawns) != 0 {
		t.Fatalf("инстанс не должен запускаться: %v", f.spawns)
	}
}

func TestSessionEndReleasesInMainCheckout(t *testing.T) {
	c, f := newTestConfig(t)
	main := mainCheckoutDir(t)
	seed(t, c, f, 15556, entry{Session: "s1", CWD: main})
	code, _, errOut := runHook(t, c, f, hookJSON(t, "SessionEnd", "s1", main, "", nil))
	if code != 0 {
		t.Fatalf("код=%d stderr=%q", code, errOut)
	}
	if _, err := os.Stat(entryPath(ctxOf(c, f.run), 15556)); !os.IsNotExist(err) {
		t.Fatal("запись должна быть удалена при завершении сессии в главном чекауте")
	}
}

func TestSessionEndWithoutInstanceSucceeds(t *testing.T) {
	c, f := newTestConfig(t)
	if code, _, _ := runHook(t, c, f, hookJSON(t, "SessionEnd", "s1", worktreeDir(t), "", nil)); code != 0 {
		t.Fatalf("освобождать нечего это успех, получен код %d", code)
	}
}

// --- коды возврата ---

func TestExitCodes(t *testing.T) {
	cwd := worktreeDir(t)
	cases := []struct {
		name  string
		input string
		want  int
	}{
		{"нечитаемый вход", "{не json", 2},
		{"PreToolUse без session_id", hookJSON(t, "PreToolUse", "", cwd, "Bash", map[string]any{"command": "adb install x"}), 2},
		{"Bash без команды", hookJSON(t, "PreToolUse", "s1", cwd, "Bash", nil), 2},
		{"SessionEnd без session_id", hookJSON(t, "SessionEnd", "", cwd, "", nil), 1},
		{"необрабатываемое событие", hookJSON(t, "Notification", "", cwd, "", nil), 0},
		{"чужой инструмент", hookJSON(t, "PreToolUse", "s1", cwd, "Read", nil), 0},
	}
	for _, tc := range cases {
		c, f := newTestConfig(t)
		code, out, errOut := runHook(t, c, f, tc.input)
		if code != tc.want {
			t.Errorf("%s: код=%d, ожидался %d (stderr=%q)", tc.name, code, tc.want, errOut)
		}
		if tc.want != 0 && errOut == "" {
			t.Errorf("%s: причина должна быть на stderr", tc.name)
		}
		if tc.want == 0 && out != "" {
			t.Errorf("%s: ожидалось молчание, получено %q", tc.name, out)
		}
	}
}

// --- распознавание ---

func TestNoInstanceForProbesAndUnrelated(t *testing.T) {
	cwd := worktreeDir(t)
	quiet := []string{
		"adb devices",
		"adb devices -l",
		"  adb version  ",
		"adb start-server",
		"echo hello",
		`echo "start the emulator now"`,
		"grep -r adb-something .",
	}
	for _, cmd := range quiet {
		c, f := newTestConfig(t)
		code, out, errOut := runHook(t, c, f, bashJSON(t, "s1", cwd, cmd))
		if code != 0 || out != "" {
			t.Errorf("%q: ожидалось молчание, получено код=%d out=%q err=%q", cmd, code, out, errOut)
		}
		if len(f.spawns) != 0 {
			t.Errorf("%q: инстанс выдан напрасно", cmd)
		}
	}
}

func TestMobileMCPListDoesNotAcquire(t *testing.T) {
	c, f := newTestConfig(t)
	in := hookJSON(t, "PreToolUse", "s1", worktreeDir(t), "mcp__mobile-mcp__mobile_list_available_devices", nil)
	code, out, _ := runHook(t, c, f, in)
	if code != 0 || out != "" || len(f.spawns) != 0 {
		t.Fatalf("перечисление устройств не должно стоить загрузки: код=%d out=%q spawns=%v", code, out, f.spawns)
	}
}

func TestReachRecognition(t *testing.T) {
	cwd := worktreeDir(t)
	reaching := []string{
		"adb install app.apk",
		"cd app && adb install app.apk",
		"nohup emulator @other -read-only &",
		"./gradlew installDebug",
		"./gradlew connectedAndroidTest",
	}
	for _, cmd := range reaching {
		c, f := newTestConfig(t)
		seed(t, c, f, 15556, entry{Session: "s1", CWD: cwd})
		code, out, errOut := runHook(t, c, f, bashJSON(t, "s1", cwd, cmd))
		if code != 0 || out == "" {
			t.Errorf("%q: обращение не распознано: код=%d out=%q err=%q", cmd, code, out, errOut)
		}
	}
}

// --- запреты ---

func TestDenyAdbRoot(t *testing.T) {
	cwd := worktreeDir(t)
	for _, cmd := range []string{"adb root", "adb -s emulator-5556 root", "cd app && adb root"} {
		c, f := newTestConfig(t)
		seed(t, c, f, 15556, entry{Session: "s1", CWD: cwd})
		code, out, _ := runHook(t, c, f, bashJSON(t, "s1", cwd, cmd))
		got := decode(t, out)
		if code != 0 || got.PermissionDecision != "deny" {
			t.Fatalf("%q: ожидался запрет, получено код=%d %+v", cmd, code, got)
		}
		if !strings.Contains(got.PermissionDecisionReason, "su 0") {
			t.Errorf("%q: причина должна называть работающую замену: %q", cmd, got.PermissionDecisionReason)
		}
	}
}

func TestDenyWritableLaunchOfPooledAVD(t *testing.T) {
	cwd := worktreeDir(t)
	cases := []struct {
		cmd  string
		deny bool
	}{
		{"emulator @pooled_avd -no-window", true},
		{"emulator -avd pooled_avd", true},
		{"nohup emulator @pooled_avd &", true},
		{"emulator @pooled_avd -read-only", false},
		{"emulator @other_avd", false},
	}
	for _, tc := range cases {
		c, f := newTestConfig(t)
		seed(t, c, f, 15556, entry{Session: "s1", CWD: cwd})
		_, out, _ := runHook(t, c, f, bashJSON(t, "s1", cwd, tc.cmd))
		got := decode(t, out)
		isDeny := got.PermissionDecision == "deny"
		if isDeny != tc.deny {
			t.Errorf("%q: запрет=%v, ожидалось %v (%+v)", tc.cmd, isDeny, tc.deny, got)
		}
		if isDeny && strings.Contains(got.PermissionDecisionReason, "plans/") {
			t.Errorf("%q: причина ссылается на несуществующий файл: %q", tc.cmd, got.PermissionDecisionReason)
		}
	}
}

func TestDenyHappensBeforeAcquire(t *testing.T) {
	c, f := newTestConfig(t)
	c.Max = 1
	cwd := worktreeDir(t)
	seed(t, c, f, 15556, entry{Session: "other", OwnerPID: 777, CWD: "/elsewhere"})
	f.starts[777] = "Wed_Aug_20_10:00:00_2026"

	_, out, _ := runHook(t, c, f, bashJSON(t, "s1", cwd, "adb root"))
	got := decode(t, out)
	if !strings.Contains(got.PermissionDecisionReason, "adb root") {
		t.Fatalf("при исчерпанном пуле причина должна быть о запрете команды, а не об исчерпании: %q", got.PermissionDecisionReason)
	}
	if len(f.spawns) != 0 {
		t.Fatal("запрещённый вызов не должен приводить к запуску эмулятора")
	}
}

// Отказ на время загрузки идёт тем же путём, что и остальные отказы выдачи:
// прибитая по внешнему пределу обработка вызов не блокирует, поэтому решение
// обязано быть явным.
func TestBootingInstanceDeniesWithRetry(t *testing.T) {
	c, f := newTestConfig(t)
	c.AcquireWait = 20 * time.Second
	cwd := worktreeDir(t)
	// Инстанс этой сессии ещё загружается: процесс жив, устройства нет.
	if err := writeEntry(ctxOf(c, f.run), 15556, entry{Session: "s1", Since: f.now, CWD: cwd, Image: c.AVD}); err != nil {
		t.Fatal(err)
	}
	f.qemu[15556] = 99999

	_, out, _ := runHook(t, c, f, bashJSON(t, "s1", cwd, "adb install app.apk"))
	got := decode(t, out)
	if got.PermissionDecision != "deny" {
		t.Fatalf("вызов должен быть запрещён, а не исполнен без привязки: %+v", got)
	}
	if !strings.Contains(got.PermissionDecisionReason, "retry") || !strings.Contains(got.PermissionDecisionReason, "emulator-15556") {
		t.Errorf("причина должна называть инстанс и предлагать повторить: %q", got.PermissionDecisionReason)
	}
	if _, ok := readEntry(ctxOf(c, f.run), 15556); !ok {
		t.Error("отказ не должен уносить с собой начатую загрузку")
	}
}

func TestExhaustedPoolDenies(t *testing.T) {
	c, f := newTestConfig(t)
	c.Max = 1
	cwd := worktreeDir(t)
	seed(t, c, f, 15556, entry{Session: "other", OwnerPID: 777, CWD: "/elsewhere"})
	f.starts[777] = "Wed_Aug_20_10:00:00_2026"

	_, out, _ := runHook(t, c, f, bashJSON(t, "s1", cwd, "adb install app.apk"))
	got := decode(t, out)
	if got.PermissionDecision != "deny" {
		t.Fatalf("невозможность выдать инстанс должна запрещать вызов: %+v", got)
	}
	if !strings.Contains(got.PermissionDecisionReason, "busy") || !strings.Contains(got.PermissionDecisionReason, "emulator-15556") {
		t.Errorf("причина должна содержать состояние пула: %q", got.PermissionDecisionReason)
	}
}

// --- подстановка ---

func TestBashSubstitution(t *testing.T) {
	c, f := newTestConfig(t)
	cwd := worktreeDir(t)
	seed(t, c, f, 15556, entry{Session: "s1", CWD: cwd})

	cmd := "cd app && adb -s emulator-9999 install x.apk && adb -e shell ls && adb -s emulator-8888 logcat"
	_, out, _ := runHook(t, c, f, bashJSON(t, "s1", cwd, cmd))
	got := decode(t, out)
	updated, _ := got.UpdatedInput["command"].(string)
	if len(got.UpdatedInput) != 1 {
		t.Errorf("во вводе была только команда, а решение содержит %v", got.UpdatedInput)
	}

	if strings.Contains(updated, "emulator-9999") || strings.Contains(updated, "emulator-8888") {
		t.Errorf("заменены не все вхождения: %q", updated)
	}
	if strings.Count(updated, "-s emulator-15556") != 3 {
		t.Errorf("ожидалось три подстановки серийника (два -s и одно -e): %q", updated)
	}
	if !strings.HasPrefix(updated, "export ANDROID_SERIAL=emulator-15556 ADB_SERIAL=emulator-15556; ") {
		t.Errorf("переменные окружения должны объявляться через export с точкой с запятой: %q", updated)
	}
	if !strings.Contains(got.AdditionalContext, "emulator-15556") {
		t.Errorf("модели должен сообщаться серийник: %q", got.AdditionalContext)
	}
}

// Имя события в решении — не украшение: Claude Code разбирает hookSpecificOutput
// размеченным объединением по этому полю и без него отбрасывает решение целиком,
// молча пропуская вызов на произвольное устройство. Отсутствие поля стоило
// Go-версии всей привязки, поэтому проверяется отдельно от содержимого решения.
func TestEveryDecisionNamesTheEvent(t *testing.T) {
	cwd := worktreeDir(t)
	cases := []struct {
		name string
		in   func(t *testing.T) string
	}{
		{"подстановка в команду", func(t *testing.T) string { return bashJSON(t, "s1", cwd, "adb install x.apk") }},
		{"подмена устройства Mobile-MCP", func(t *testing.T) string {
			return hookJSON(t, "PreToolUse", "s1", cwd, "mcp__mobile-mcp__mobile_take_screenshot", nil)
		}},
		{"отказ", func(t *testing.T) string { return bashJSON(t, "s1", cwd, "adb root") }},
	}
	for _, tc := range cases {
		c, f := newTestConfig(t)
		seed(t, c, f, 15556, entry{Session: "s1", CWD: cwd})
		_, out, _ := runHook(t, c, f, tc.in(t))
		if got := decode(t, out).HookEventName; got != "PreToolUse" {
			t.Errorf("%s: hookEventName=%q, ожидалось PreToolUse", tc.name, got)
		}
	}
}

// Обёртки проекта аргументов не получают: серийник приходит переменными
// окружения, и разбирать чужой формат аргументов роутер не должен.
func TestWrapperGetsNoAppendedArguments(t *testing.T) {
	cwd := t.TempDir()
	if err := os.WriteFile(filepath.Join(cwd, ".git"), []byte("gitdir: /elsewhere\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	writeSettings(t, cwd, `{"avd": "pooled_avd", "reach": ["wrapper\\.sh"]}`)
	c, f := newTestConfig(t)
	seed(t, c, f, 15556, entry{Session: "s1", CWD: cwd})
	_, out, _ := runHook(t, c, f, bashJSON(t, "s1", cwd, "./wrapper.sh check"))
	updated, _ := decode(t, out).UpdatedInput["command"].(string)
	if !strings.HasSuffix(updated, "./wrapper.sh check") {
		t.Errorf("к вызову обёртки не должно дописываться ничего: %q", updated)
	}
	if !strings.Contains(updated, "ANDROID_SERIAL=emulator-15556") {
		t.Errorf("привязка обеспечивается переменными окружения: %q", updated)
	}
}

func TestMobileMCPDeviceRewrite(t *testing.T) {
	cwd := worktreeDir(t)
	for _, ti := range []map[string]any{nil, {"device": "emulator-9999"}} {
		c, f := newTestConfig(t)
		seed(t, c, f, 15556, entry{Session: "s1", CWD: cwd})
		in := hookJSON(t, "PreToolUse", "s1", cwd, "mcp__mobile-mcp__mobile_take_screenshot", ti)
		_, out, _ := runHook(t, c, f, in)
		if got := decode(t, out).UpdatedInput["device"]; got != "emulator-15556" {
			t.Errorf("tool_input=%v: device=%q, ожидался emulator-15556", ti, got)
		}
	}
}

// Изменённый ввод заменяет ввод вызова целиком, а не сливается с ним по полям,
// поэтому потерянный обязательный аргумент делает вызов негодным по схеме самого
// инструмента, а выдуманное поле неотличимо от заданного вызывающей стороной.
func TestMobileMCPKeepsOtherArguments(t *testing.T) {
	cwd := worktreeDir(t)
	// Числа и булево значение проходят через JSON, поэтому строковая карта их не
	// унесла бы: тип аргумента задаёт схема инструмента, а не роутер.
	for _, tc := range []struct{ in, want map[string]any }{
		{
			in:   map[string]any{"device": "emulator-9999", "x": 100.0, "y": 200.5, "submit": true, "text": "hi"},
			want: map[string]any{"device": "emulator-15556", "x": 100.0, "y": 200.5, "submit": true, "text": "hi"},
		},
		{in: nil, want: map[string]any{"device": "emulator-15556"}},
	} {
		c, f := newTestConfig(t)
		seed(t, c, f, 15556, entry{Session: "s1", CWD: cwd})
		in := hookJSON(t, "PreToolUse", "s1", cwd, "mcp__mobile-mcp__mobile_click_on_screen_at_coordinates", tc.in)
		_, out, _ := runHook(t, c, f, in)
		if got := decode(t, out).UpdatedInput; !reflect.DeepEqual(got, tc.want) {
			t.Errorf("tool_input=%v: решение содержит %v, ожидалось %v", tc.in, got, tc.want)
		}
	}
}

// Признак фонового исполнения и предел времени меняют способ исполнения команды:
// потеряв их, роутер превращает фоновую сборку в синхронную.
func TestBashKeepsOtherArguments(t *testing.T) {
	c, f := newTestConfig(t)
	cwd := worktreeDir(t)
	seed(t, c, f, 15556, entry{Session: "s1", CWD: cwd})

	in := hookJSON(t, "PreToolUse", "s1", cwd, "Bash", map[string]any{
		"command":           "adb logcat",
		"run_in_background": true,
		"timeout":           600000.0,
		"description":       "read the log",
	})
	_, out, _ := runHook(t, c, f, in)
	updated := decode(t, out).UpdatedInput

	for k, want := range map[string]any{"run_in_background": true, "timeout": 600000.0, "description": "read the log"} {
		if updated[k] != want {
			t.Errorf("поле %q не дошло без изменений: решение содержит %v", k, updated)
		}
	}
	if cmd, _ := updated["command"].(string); !strings.Contains(cmd, "emulator-15556") {
		t.Errorf("команда не привязана к инстансу: %q", cmd)
	}
	if len(updated) != 4 {
		t.Errorf("состав полей изменился: %v", updated)
	}
}

// --- пул ---

func TestAcquireIdempotentAndIsolated(t *testing.T) {
	c, f := newTestConfig(t)
	f.starts[100], f.starts[200] = "Wed_10:00:00", "Wed_11:00:00"

	first, err := acquire(ctxOf(c, f.run), "s1", 100, "/w1")
	if err != nil {
		t.Fatal(err)
	}
	again, err := acquire(ctxOf(c, f.run), "s1", 100, "/w1")
	if err != nil {
		t.Fatal(err)
	}
	if again != first {
		t.Errorf("выдача не идемпотентна: %q затем %q", first, again)
	}
	sub, err := acquire(ctxOf(c, f.run), "subagent-session", 100, "/w1")
	if err != nil {
		t.Fatal(err)
	}
	if sub != first {
		t.Errorf("субагент с тем же владельцем должен получить тот же инстанс: %q", sub)
	}
	other, err := acquire(ctxOf(c, f.run), "s2", 200, "/w2")
	if err != nil {
		t.Fatal(err)
	}
	if other == first {
		t.Errorf("два владельца получили один серийник: %q", other)
	}
	if len(f.spawns) != 2 {
		t.Errorf("ожидалось два запуска, получено %v", f.spawns)
	}
}

// Своим считается только инстанс запрошенного образа. Иначе смена образа в
// настройках проекта оставляет сессию работать с прежним образом молча: серийник
// тот же, устройство отвечает, и отличить это от исправной работы нечем.
func TestInstanceOfAnotherImageIsNotHandedBack(t *testing.T) {
	c, f := newTestConfig(t)
	f.avds = []string{"old_avd", "pooled_avd"}
	c.AVD = "old_avd"
	if _, err := acquire(ctxOf(c, f.run), "s1", 0, "/w1"); err != nil {
		t.Fatal(err)
	}

	c.AVD = "pooled_avd" // в настройках проекта задали другой образ
	serial, err := acquire(ctxOf(c, f.run), "s1", 0, "/w1")
	if err != nil {
		t.Fatal(err)
	}
	if len(f.spawns) != 2 {
		t.Fatalf("инстанс запрошенного образа должен быть запущен: %v", f.spawns)
	}
	if ports := entries(ctxOf(c, f.run)); len(ports) != 1 {
		t.Errorf("прежний инстанс должен быть освобождён, в реестре %v", ports)
	}
	if serial != "emulator-15556" {
		t.Errorf("должен возвращаться серийник нового инстанса: %q", serial)
	}
	if e, ok := readEntry(ctxOf(c, f.run), 15556); !ok || e.Image != "pooled_avd" {
		t.Errorf("запись должна помнить запрошенный образ: %+v", e)
	}
}

// Записи, сделанные до того, как образ стал записываться, его не содержат:
// образ неизвестен, значит это инстанс другого образа. Иначе такая запись
// держалась бы сессией до конца, ни разу не совпав с настройками.
func TestEntryWithoutImageIsNotHandedBack(t *testing.T) {
	c, f := newTestConfig(t)
	if err := writeEntry(ctxOf(c, f.run), 15556, entry{Session: "s1", Since: f.now, CWD: "/w1"}); err != nil {
		t.Fatal(err)
	}
	f.devices[15556], f.qemu[15556] = "device", 99999

	serial, err := acquire(ctxOf(c, f.run), "s1", 0, "/w1")
	if err != nil {
		t.Fatal(err)
	}
	if serial != "emulator-15556" || len(f.spawns) != 1 {
		t.Errorf("запись без образа должна быть освобождена и заменена запуском: %q %v", serial, f.spawns)
	}
}

func TestAcquireRespectsCap(t *testing.T) {
	c, f := newTestConfig(t)
	c.Max = 2
	f.starts[100], f.starts[200] = "a", "b"
	if _, err := acquire(ctxOf(c, f.run), "s1", 100, "/w1"); err != nil {
		t.Fatal(err)
	}
	if _, err := acquire(ctxOf(c, f.run), "s2", 200, "/w2"); err != nil {
		t.Fatal(err)
	}
	_, err := acquire(ctxOf(c, f.run), "s3", 300, "/w3")
	if err == nil {
		t.Fatal("ожидалась ошибка исчерпания предела")
	}
	if !strings.Contains(err.Error(), "emulator-15556") {
		t.Errorf("ошибка должна содержать состояние пула: %v", err)
	}
	if err := release(ctxOf(c, f.run), "s1", 100); err != nil {
		t.Fatal(err)
	}
	if _, err := acquire(ctxOf(c, f.run), "s3", 300, "/w3"); err != nil {
		t.Fatalf("после освобождения инстанс должен выдаваться: %v", err)
	}
}

func TestAcquirePicksPortsInStepsOfTwo(t *testing.T) {
	c, f := newTestConfig(t)
	if _, err := acquire(ctxOf(c, f.run), "s1", 0, "/w1"); err != nil {
		t.Fatal(err)
	}
	if _, err := acquire(ctxOf(c, f.run), "s2", 0, "/w2"); err != nil {
		t.Fatal(err)
	}
	want := []int{15556, 15558}
	if fmt.Sprint(f.spawns) != fmt.Sprint(want) {
		t.Errorf("порты выбраны как %v, ожидалось %v", f.spawns, want)
	}
}

func TestAcquireRequiresPreparedAVD(t *testing.T) {
	c, f := newTestConfig(t)
	f.avds = nil
	_, err := acquire(ctxOf(c, f.run), "s1", 0, "/w1")
	if err == nil || !strings.Contains(err.Error(), "prepare") {
		t.Fatalf("ожидалась ошибка с указанием подготовить образ, получено %v", err)
	}
	if len(f.spawns) != 0 {
		t.Fatal("образ не готов, запускать нечего")
	}
}

func TestAcquireFailsWhenBootTimesOut(t *testing.T) {
	c, f := newTestConfig(t)
	// Инстанс не приходит в готовность: spawn не поднимает устройство.
	c.Spawn = func(logPath, name string, args ...string) error { f.spawns = append(f.spawns, 0); return nil }
	_, err := acquire(ctxOf(c, f.run), "s1", 0, "/w1")
	if err == nil || !strings.Contains(err.Error(), "did not boot") {
		t.Fatalf("ожидалась ошибка загрузки, получено %v", err)
	}
	if len(entries(ctxOf(c, f.run))) != 0 {
		t.Error("место должно быть освобождено при неудачной загрузке")
	}
}

func TestDeadInstanceEntryIsNotHandedBack(t *testing.T) {
	c, f := newTestConfig(t)
	if err := writeEntry(ctxOf(c, f.run), 15556, entry{Session: "s1", Since: f.now, CWD: "/w1", Image: c.AVD}); err != nil {
		t.Fatal(err)
	}
	// Записи есть, инстанса нет: устройства и процесса не существует.
	serial, err := acquire(ctxOf(c, f.run), "s1", 0, "/w1")
	if err != nil {
		t.Fatal(err)
	}
	if serial != "emulator-15556" || len(f.spawns) != 1 {
		t.Errorf("мёртвая запись должна быть утилизирована и заменена запуском: %q %v", serial, f.spawns)
	}
}

func TestOfflineInstanceWithLiveProcessIsRecovered(t *testing.T) {
	c, f := newTestConfig(t)
	seed(t, c, f, 15556, entry{Session: "s1", CWD: "/w1"})
	f.devices[15556] = "offline"
	// Восстановление: при третьей проверке устройство отвечает.
	calls := 0
	adbPath, base := adb(ctxOf(c, f.run)), f.run
	stub := func(name string, args ...string) (string, error) {
		if name == adbPath && len(args) > 0 && args[0] == "devices" {
			if calls++; calls > 2 {
				f.devices[15556] = "device"
			}
		}
		return base(name, args...)
	}
	serial, err := acquire(ctxOf(c, stub), "s1", 0, "/w1")
	if err != nil {
		t.Fatal(err)
	}
	if serial != "emulator-15556" {
		t.Errorf("временно недоступный инстанс не должен утилизироваться: %q", serial)
	}
	if len(f.spawns) != 0 {
		t.Error("восстановленный инстанс не требует запуска нового")
	}
}

// --- уборка ---

// Загрузка, начатую предыдущим обращением, повторное обращение не должно
// утилизировать: иначе каждый повтор начинает её заново и пул не приходит в
// рабочее состояние никогда.
func TestBootingInstanceIsNotUtilized(t *testing.T) {
	c, f := newTestConfig(t)
	// Запись свежая, процесс инстанса жив, устройства ещё нет: идёт загрузка.
	if err := writeEntry(ctxOf(c, f.run), 15556, entry{Session: "s1", Since: f.now, CWD: "/w1", Image: c.AVD}); err != nil {
		t.Fatal(err)
	}
	f.qemu[15556] = 99999
	c.AcquireWait = 20 * time.Second // бюджет кончится раньше загрузки

	_, err := acquire(ctxOf(c, f.run), "s1", 0, "/w1")
	if err == nil || !strings.Contains(err.Error(), "retry") {
		t.Fatalf("ожидался отказ с предложением повторить, получено %v", err)
	}
	if _, ok := readEntry(ctxOf(c, f.run), 15556); !ok {
		t.Error("запись загружающегося инстанса не должна удаляться")
	}
	if f.qemu[15556] == 0 {
		t.Error("процесс загружающегося инстанса не должен завершаться")
	}
	if len(f.spawns) != 0 {
		t.Errorf("второй инстанс запускать нельзя: %v", f.spawns)
	}
}

// Загрузка, не уложившаяся во время, отведённое на восстановление, всё равно
// остаётся загрузкой: её нужно подобрать, а не начать заново.
func TestBootInProgressIsAdoptedNotRestarted(t *testing.T) {
	c, f := newTestConfig(t)
	if err := writeEntry(ctxOf(c, f.run), 15556, entry{Session: "s1", Since: f.now, CWD: "/w1", Image: c.AVD}); err != nil {
		t.Fatal(err)
	}
	f.qemu[15556] = 99999
	// Устройство появляется много позже, чем отведено на восстановление связи.
	calls := 0
	adbPath, base := adb(ctxOf(c, f.run)), f.run
	stub := func(name string, args ...string) (string, error) {
		if name == adbPath && len(args) > 0 && args[0] == "devices" {
			if calls++; calls > 20 {
				f.devices[15556] = "device"
			}
		}
		return base(name, args...)
	}

	serial, err := acquire(ctxOf(c, stub), "s1", 0, "/w1")
	if err != nil {
		t.Fatal(err)
	}
	if serial != "emulator-15556" {
		t.Errorf("должен возвращаться загрузившийся инстанс: %q", serial)
	}
	if len(f.spawns) != 0 {
		t.Errorf("начатая загрузка подбирается, а не начинается заново: %v", f.spawns)
	}
}

// Погибший при запуске инстанс не должен держать взаимное исключение запусков
// всё время, отведённое загрузке: этим временем оплачивают ожидание остальные.
func TestReadinessWaitStopsWhenProcessDies(t *testing.T) {
	c, f := newTestConfig(t)
	// Запуск не поднимает ни устройства, ни процесса.
	c.Spawn = func(logPath, name string, args ...string) error {
		f.spawns = append(f.spawns, 0)
		return nil
	}
	start := f.now

	_, err := acquire(ctxOf(c, f.run), "s1", 0, "/w1")
	if err == nil || !strings.Contains(err.Error(), "did not boot") {
		t.Fatalf("ожидалась ошибка загрузки, получено %v", err)
	}
	if waited := f.now - start; waited >= int64(c.BootTimeout.Seconds()) {
		t.Errorf("ожидание должно прекращаться при гибели процесса, прошло %d с", waited)
	}
	if len(entries(ctxOf(c, f.run))) != 0 {
		t.Error("место должно быть освобождено")
	}
}

// Бюджет выдачи меньше предела, отведённого вызывающей стороной: за её пределом
// обработку прерывают, а прерванная обработка вызов не блокирует.
func TestAcquireDeniesWhenBudgetRunsOut(t *testing.T) {
	c, f := newTestConfig(t)
	c.AcquireWait = 10 * time.Second

	// Взаимное исключение запусков держит кто-то другой.
	held, err := os.OpenFile(filepath.Join(c.Registry, ".launch.lock"), os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	if err := syscall.Flock(int(held.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		t.Fatal(err)
	}
	defer func() { syscall.Flock(int(held.Fd()), syscall.LOCK_UN); held.Close() }()

	start := f.now
	if _, err := acquire(ctxOf(c, f.run), "s1", 0, "/w1"); err == nil || !strings.Contains(err.Error(), "retry") {
		t.Fatalf("ожидался отказ с предложением повторить, получено %v", err)
	}
	if waited := f.now - start; waited > int64(c.AcquireWait.Seconds())+4 {
		t.Errorf("ожидание не должно выходить за бюджет выдачи, прошло %d с", waited)
	}
	if len(f.spawns) != 0 {
		t.Errorf("инстанс запускать было нельзя: %v", f.spawns)
	}
}

func TestReapSparesBootingEntry(t *testing.T) {
	c, f := newTestConfig(t)
	// Живой владелец, процесса инстанса ещё нет, запись свежая.
	if err := writeEntry(ctxOf(c, f.run), 15556, entry{Session: "s1", OwnerPID: 100, OwnerStart: "a", CWD: "/w1", Since: f.now - 10}); err != nil {
		t.Fatal(err)
	}
	f.starts[100] = "a"
	reap(ctxOf(c, f.run))
	if _, err := os.Stat(entryPath(ctxOf(c, f.run), 15556)); err != nil {
		t.Fatal("запись загружающегося инстанса не должна убираться")
	}
}

func TestReapRemovesOrphans(t *testing.T) {
	c, f := newTestConfig(t)
	f.starts[100] = "a"
	// Живой владелец, инстанс погиб, запись старше времени загрузки.
	if err := writeEntry(ctxOf(c, f.run), 15556, entry{Session: "s1", OwnerPID: 100, OwnerStart: "a", Since: f.now - 400}); err != nil {
		t.Fatal(err)
	}
	// Мёртвый владелец, запись свежая.
	f.dead[200] = true
	if err := writeEntry(ctxOf(c, f.run), 15558, entry{Session: "s2", OwnerPID: 200, OwnerStart: "b", Since: f.now}); err != nil {
		t.Fatal(err)
	}
	// Владелец жив, инстанс жив: остаётся.
	seed(t, c, f, 15560, entry{Session: "s3", OwnerPID: 100, OwnerStart: "a", Since: f.now - 400})

	reap(ctxOf(c, f.run))
	if got := entries(ctxOf(c, f.run)); len(got) != 1 || got[0] != 15560 {
		t.Fatalf("после уборки в реестре %v, ожидался только 15560", got)
	}
}

// Нечитаемая запись перестаёт быть бессмертной: использовать её не может никто,
// а место в пределе и порт она держит. Времени создания у неё нет, поэтому запас
// на загрузку отмеряется по времени изменения файла.
func TestReapRemovesUnreadableEntry(t *testing.T) {
	c, f := newTestConfig(t)
	unreadableEntry(t, c, f, 15556, time.Unix(f.now, 0).Add(-c.BootTimeout-time.Minute))
	reap(ctxOf(c, f.run))
	if got := entries(ctxOf(c, f.run)); len(got) != 0 {
		t.Fatalf("нечитаемая запись должна убираться, в реестре %v", got)
	}
	if !portFree(ctxOf(c, f.run), 15556) {
		t.Error("порт нечитаемой записи должен освобождаться")
	}
}

// Запись могли создавать прямо сейчас: свежая нечитаемая запись — это половина
// записи, а не мусор.
func TestReapSparesJustWrittenUnreadableEntry(t *testing.T) {
	c, f := newTestConfig(t)
	unreadableEntry(t, c, f, 15556, time.Unix(f.now, 0).Add(-10*time.Second))
	reap(ctxOf(c, f.run))
	if got := entries(ctxOf(c, f.run)); len(got) != 1 {
		t.Fatalf("свежая нечитаемая запись должна сохраняться, в реестре %v", got)
	}
}

// unreadableEntry кладёт в реестр файл, который не удалось бы прочитать: так
// выглядит запись процесса, умершего до того, как он её дописал.
func unreadableEntry(t *testing.T, c config, f *fake, port int, mtime time.Time) {
	t.Helper()
	path := entryPath(ctxOf(c, f.run), port)
	if err := os.WriteFile(path, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(path, mtime, mtime); err != nil {
		t.Fatal(err)
	}
}

func TestReapSparesInstancesOutsideOwnRegistry(t *testing.T) {
	c, f := newTestConfig(t)
	f.qemu[15600] = 91234 // инстанс, которого нет в нашем реестре
	reap(ctxOf(c, f.run))
	if _, ok := f.qemu[15600]; !ok {
		t.Fatal("уборка не должна завершать инстансы, отсутствующие в собственном реестре")
	}
}

func TestReapSweepsUnusedCOWLayers(t *testing.T) {
	c, f := newTestConfig(t)
	unused := filepath.Join(c.COWDir, "emulator-15556.qcow2")
	inUse := filepath.Join(c.COWDir, "emulator-15558.qcow2")
	for _, p := range []string{unused, inUse} {
		if err := os.WriteFile(p, []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	f.open[inUse] = true
	reap(ctxOf(c, f.run))
	if _, err := os.Stat(unused); !os.IsNotExist(err) {
		t.Error("неиспользуемый слой должен удаляться")
	}
	if _, err := os.Stat(inUse); err != nil {
		t.Error("используемый слой должен сохраняться")
	}
}

// Отсутствие ответа о занятости — не ответ «свободен»: когда проверять нечем,
// слой сохраняется.
func TestReapKeepsCOWLayerWhenOccupancyCannotBeChecked(t *testing.T) {
	c, f := newTestConfig(t)
	layer := filepath.Join(c.COWDir, "emulator-15556.qcow2")
	if err := os.WriteFile(layer, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	f.noLsof = true
	reap(ctxOf(c, f.run))
	if _, err := os.Stat(layer); err != nil {
		t.Error("слой должен сохраняться, когда занятость проверить нечем")
	}
}

// --- реестр и владелец ---

func TestEntrySurvivesSpacesInPaths(t *testing.T) {
	c, f := newTestConfig(t)
	want := entry{Session: "s 1", OwnerPID: 100, OwnerStart: "Wed Aug 20 10:00:00 2026",
		CWD: "/Users/me/my work/project dir", Since: f.now, Image: "pooled avd"}
	if err := writeEntry(ctxOf(c, f.run), 15556, want); err != nil {
		t.Fatal(err)
	}
	got, ok := readEntry(ctxOf(c, f.run), 15556)
	if !ok || got != want {
		t.Fatalf("запись прочитана как %+v, ожидалась %+v", got, want)
	}
}

// Читатель, открывший файл записи до перезаписи, обязан увидеть прежнюю запись
// целиком. Перезапись на месте проходит через усечённое состояние, в котором
// запись неотличима от нечитаемой: она держит порт и место в пределе, а сессии
// не достаётся. Замена файла целиком показать это состояние не может.
func TestEntryIsReplacedWholeOrNotAtAll(t *testing.T) {
	c, f := newTestConfig(t)
	first := entry{Session: "s1", OwnerPID: 100, CWD: "/w1", Since: f.now}
	if err := writeEntry(ctxOf(c, f.run), 15556, first); err != nil {
		t.Fatal(err)
	}
	reader, err := os.Open(entryPath(ctxOf(c, f.run), 15556))
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()

	second := entry{Session: "s2", OwnerPID: 200, CWD: "/w2", Since: f.now + 1}
	if err := writeEntry(ctxOf(c, f.run), 15556, second); err != nil {
		t.Fatal(err)
	}

	b, err := io.ReadAll(reader)
	if err != nil {
		t.Fatal(err)
	}
	var got entry
	if err := json.Unmarshal(b, &got); err != nil || got != first {
		t.Errorf("читатель прежней записи увидел %+v (%v), ожидалась %+v", got, err, first)
	}
	if got, ok := readEntry(ctxOf(c, f.run), 15556); !ok || got != second {
		t.Errorf("по пути записи читается %+v, ожидалась %+v", got, second)
	}
}

func TestOwnerIdentityIncludesStartTime(t *testing.T) {
	c, f := newTestConfig(t)
	f.starts[100] = "Wed_Aug_20_11:00:00_2026" // номер переиспользован другим процессом
	if ownerAlive(ctxOf(c, f.run), entry{OwnerPID: 100, OwnerStart: "Wed_Aug_20_10:00:00_2026"}) {
		t.Error("при ином времени запуска владелец записи должен считаться мёртвым")
	}
	if !ownerAlive(ctxOf(c, f.run), entry{OwnerPID: 100, OwnerStart: "Wed_Aug_20_11:00:00_2026"}) {
		t.Error("владелец с совпадающим временем запуска жив")
	}
}

func TestOwnerIsNearestClaudeAncestor(t *testing.T) {
	c, f := newTestConfig(t)
	self := os.Getpid()
	f.ppid[self] = 500
	f.comm[500] = "-zsh"
	f.ppid[500] = 600
	f.comm[600] = "claude"
	if got := ownerPID(ctxOf(c, f.run)); got != 600 {
		t.Errorf("владелец определён как %d, ожидался 600", got)
	}
}

func TestOwnerUndefinedWhenNoSessionProcess(t *testing.T) {
	c, f := newTestConfig(t)
	f.ppid[os.Getpid()] = 1
	if got := ownerPID(ctxOf(c, f.run)); got != 0 {
		t.Errorf("без процесса сессии владелец не определён, получено %d", got)
	}
}

// --- освобождение ---

func TestReleaseKillsInstanceAndRemovesLog(t *testing.T) {
	c, f := newTestConfig(t)
	seed(t, c, f, 15556, entry{Session: "s1", CWD: "/w1"})
	if err := os.WriteFile(logPath(ctxOf(c, f.run), 15556), []byte("log"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := release(ctxOf(c, f.run), "s1", 0); err != nil {
		t.Fatal(err)
	}
	if len(entries(ctxOf(c, f.run))) != 0 {
		t.Error("запись должна быть удалена")
	}
	if _, err := os.Stat(logPath(ctxOf(c, f.run), 15556)); !os.IsNotExist(err) {
		t.Error("журнал должен быть удалён")
	}
	if _, ok := f.qemu[15556]; ok {
		t.Error("инстанс должен быть завершён")
	}
}

func TestStatusReportsOwners(t *testing.T) {
	c, f := newTestConfig(t)
	if statusText(ctxOf(c, f.run)) != "" {
		t.Error("состояние пустого пула пусто")
	}
	f.starts[100] = "a"
	seed(t, c, f, 15556, entry{Session: "s1", OwnerPID: 100, OwnerStart: "a", CWD: "/my work"})
	f.dead[200] = true
	seed(t, c, f, 15558, entry{Session: "s2", OwnerPID: 200, OwnerStart: "b", CWD: "/other"})

	got := statusText(ctxOf(c, f.run))
	for _, want := range []string{"emulator-15556 alive 100 idle=0s /my work", "emulator-15558 dead 200 idle=0s /other"} {
		if !strings.Contains(got, want) {
			t.Errorf("состояние %q не содержит %q", got, want)
		}
	}
}

func TestBootRequiresTwoConsecutiveConfirmations(t *testing.T) {
	c, f := newTestConfig(t)
	f.devices[15556], f.qemu[15556] = "device", 99999
	// Загруженный это ещё не пригодный: одно подтверждение готовностью не является.
	replies := []string{"1", "", "1", "1"}
	calls := 0
	base := f.run
	stub := func(name string, args ...string) (string, error) {
		if len(args) > 3 && args[3] == "getprop" {
			r := replies[min(calls, len(replies)-1)]
			calls++
			if r == "" {
				return "", errors.New("device offline")
			}
			return r + "\r\n", nil
		}
		return base(name, args...)
	}
	since := c.Now()
	if waitReady(ctxOf(c, stub), 15556, since, since.Add(c.BootTimeout), since.Add(c.AcquireWait)) != bootReady {
		t.Fatal("после двух подряд подтверждений ожидалась готовность")
	}
	if calls != 4 {
		t.Errorf("проверок %d, ожидалось 4: одно подтверждение не должно завершать ожидание", calls)
	}
}

// Ради этого flock заменил каталог-лок: ядро снимает блокировку при завершении
// держателя любым способом, поэтому «пул заклинен навсегда» недостижимо.
func TestLaunchLockSurvivesKilledHolder(t *testing.T) {
	if dir := os.Getenv("LOCK_HOLDER_REGISTRY"); dir != "" {
		c := defaultConfig()
		c.Registry = dir
		if _, err := lock(ctxOf(c, execRun), c.Now().Add(c.AcquireWait)); err != nil {
			os.Exit(3)
		}
		fmt.Println("locked")
		time.Sleep(time.Minute)
		os.Exit(0)
	}

	c, f := newTestConfig(t)
	c.AcquireWait = 0 // не ждать: держатель жив, блокировка должна быть занята
	c.Sleep = time.Sleep

	holder := exec.Command(os.Args[0], "-test.run=TestLaunchLockSurvivesKilledHolder")
	holder.Env = append(os.Environ(), "LOCK_HOLDER_REGISTRY="+c.Registry)
	stdout, err := holder.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := holder.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { holder.Process.Kill(); holder.Wait() }()

	scan, took := bufio.NewScanner(stdout), false
	for scan.Scan() {
		if strings.Contains(scan.Text(), "locked") {
			took = true
			break
		}
	}
	if !took {
		t.Fatalf("держатель не взял блокировку: %v", scan.Err())
	}
	if _, err := lock(ctxOf(c, f.run), c.Now()); err == nil {
		t.Fatal("пока держатель жив, блокировка должна быть занята")
	}

	if err := holder.Process.Signal(syscall.SIGKILL); err != nil {
		t.Fatal(err)
	}
	holder.Wait()

	lk, err := lock(ctxOf(c, f.run), c.Now())
	if err != nil {
		t.Fatalf("после аварийного завершения держателя блокировка должна освободиться: %v", err)
	}
	unlock(lk)
}

func TestUnresponsiveInstanceIsForceKilled(t *testing.T) {
	c, f := newTestConfig(t)
	seed(t, c, f, 15556, entry{Session: "s1", CWD: "/w1"})
	qemu := f.qemu[15556]

	var signals []syscall.Signal
	c.Signal = func(pid int, sig syscall.Signal) error {
		if pid == qemu {
			signals = append(signals, sig)
		}
		return nil
	}
	// Запрос завершения не помогает: процесс инстанса остаётся.
	adbPath, base := adb(ctxOf(c, f.run)), f.run
	stub := func(name string, args ...string) (string, error) {
		if name == adbPath && len(args) >= 4 && args[2] == "emu" {
			return "", nil
		}
		return base(name, args...)
	}

	if err := release(ctxOf(c, stub), "s1", 0); err != nil {
		t.Fatal(err)
	}
	if len(signals) == 0 || signals[len(signals)-1] != syscall.SIGKILL {
		t.Errorf("неответивший инстанс должен завершаться принудительно, сигналы: %v", signals)
	}
}

// --- простой ---

// routerLog — содержимое журнала решений.
func routerLog(t *testing.T, c config) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(c.Registry, "router.log"))
	if err != nil {
		if os.IsNotExist(err) {
			return ""
		}
		t.Fatal(err)
	}
	return string(b)
}

// Запись без отметки — либо запись прежней версии, либо запись, о работе чьей
// сессии свидетельств нет. Простой у неё бесконечный, и уходит она первой.
func TestEntryWithoutLastSeenIsIdleForever(t *testing.T) {
	c, f := newTestConfig(t)
	ctx := ctxOf(c, f.run)
	if err := os.WriteFile(entryPath(ctx, 15556), []byte(`{"session":"s1","since":1}`), 0o644); err != nil {
		t.Fatal(err)
	}
	e, ok := readEntry(ctx, 15556)
	if !ok {
		t.Fatal("запись прежней версии должна читаться")
	}
	if got := idle(ctx, e); got <= c.IdleTimeout {
		t.Errorf("простой записи без отметки %s, ожидался больше %s", got, c.IdleTimeout)
	}
}

func TestTouchMarksOwnEntryOnly(t *testing.T) {
	c, f := newTestConfig(t)
	ctx := ctxOf(c, f.run)
	seed(t, c, f, 15556, entry{Session: "s1"})
	seed(t, c, f, 15558, entry{Session: "s2"})
	was := f.now
	f.now += 100

	touch(ctx, "s1")
	if e, _ := readEntry(ctx, 15556); e.LastSeen != f.now {
		t.Errorf("своя запись не отмечена: %d, ожидалось %d", e.LastSeen, f.now)
	}
	if e, _ := readEntry(ctx, 15558); e.LastSeen != was {
		t.Errorf("чужая запись отмечена: %d, ожидалось %d", e.LastSeen, was)
	}

	// Сессия без инстанса записи не создаёт.
	touch(ctx, "s3")
	if got := entries(ctx); len(got) != 2 {
		t.Errorf("в реестре %v, ожидались только две прежние записи", got)
	}
}

// Брошенную сессию не отличить от работающей ни по владельцу, ни по процессу
// инстанса: живы оба. Отличает только простой.
func TestSweepIdleRemovesAbandonedSession(t *testing.T) {
	c, f := newTestConfig(t)
	ctx := ctxOf(c, f.run)
	f.starts[100] = "a"
	seed(t, c, f, 15556, entry{Session: "s1", OwnerPID: 100, OwnerStart: "a", Since: f.now - 1000, LastSeen: f.now - 1000})
	seed(t, c, f, 15558, entry{Session: "s2", OwnerPID: 100, OwnerStart: "a", Since: f.now - 1000, LastSeen: f.now - 10})

	sweepIdle(ctx)

	if got := entries(ctx); len(got) != 1 || got[0] != 15558 {
		t.Fatalf("после уборки в реестре %v, ожидался только 15558", got)
	}
	if _, live := f.qemu[15556]; live {
		t.Error("инстанс простаивающей сессии должен быть завершён")
	}
	if got := routerLog(t, c); !strings.Contains(got, "emulator-15556 kill reason=idle idle=16m40s session=s1") {
		t.Errorf("журнал не называет инстанс, простой и причину: %q", got)
	}
}

// Сирота с неизвестным владельцем: живой её считает всякая проверка владельца,
// поэтому остальные виды уборки не могут тронуть её в принципе.
func TestSweepIdleRemovesEntryWithoutLastSeen(t *testing.T) {
	c, f := newTestConfig(t)
	ctx := ctxOf(c, f.run)
	if err := writeEntry(ctx, 15556, entry{Session: "s1", Since: f.now - 1000, Image: c.AVD}); err != nil {
		t.Fatal(err)
	}
	f.devices[15556], f.qemu[15556] = "device", 99999

	sweepIdle(ctx)

	if got := entries(ctx); len(got) != 0 {
		t.Fatalf("в реестре %v, ожидалась пустота", got)
	}
	// Полвека в журнале выглядели бы переполнением, а не причиной.
	if got := routerLog(t, c); !strings.Contains(got, "kill reason=idle idle=never session=s1") {
		t.Errorf("журнал %q должен называть простой записи без отметки как never", got)
	}
}

// Запись создаётся до запуска, а загрузка событий хука не порождает: свежая
// запись это не простой, а запуск, который может быть ещё не завершён.
func TestSweepIdleSparesBootingEntry(t *testing.T) {
	c, f := newTestConfig(t)
	ctx := ctxOf(c, f.run)
	if err := writeEntry(ctx, 15556, entry{Session: "s1", Since: f.now - 10, Image: c.AVD}); err != nil {
		t.Fatal(err)
	}
	sweepIdle(ctx)
	if got := entries(ctx); len(got) != 1 {
		t.Fatalf("запись загружающегося инстанса убрана: %v", got)
	}
}

// Проход по простою выполняется на каждом вызове инструмента в любом проекте
// машины, поэтому в обычном случае он обязан не делать ничего.
func TestSweepIdleRunsNoCommandsWhenNothingIsStale(t *testing.T) {
	c, f := newTestConfig(t)
	calls := 0
	counting := func(name string, args ...string) (string, error) {
		calls++
		return f.run(name, args...)
	}
	ctx := ctxOf(c, counting)
	seed(t, c, f, 15556, entry{Session: "s1", Since: f.now - 1000, LastSeen: f.now - 10})

	sweepIdle(ctx)

	if calls != 0 {
		t.Errorf("проход запустил %d внешних команд, ожидалось ни одной", calls)
	}
	if got := entries(ctx); len(got) != 1 {
		t.Fatalf("в реестре %v, ожидалась одна запись", got)
	}
}

// Ожидание готовности это активность: событий хука сессия в это время не
// порождает, а ждать может дольше, чем отведено простою. Без отметки внутри
// ожидания инстанс завершила бы уборка из чужого процесса — прямо под тем
// обращением, которое его ждёт.
func TestWaitingForReadinessCountsAsActivity(t *testing.T) {
	c, f := newTestConfig(t)
	c.IdleTimeout = 60 * time.Second
	start := f.now
	// Запись старше времени загрузки: запас на незавершённый запуск её не
	// защищает, защищает только отметка.
	ctx := ctxOf(c, f.run)
	if err := writeEntry(ctx, 15556, entry{Session: "s1", Since: start - 1000, LastSeen: start, Image: c.AVD}); err != nil {
		t.Fatal(err)
	}
	f.qemu[15556] = 99999
	f.devices[15556] = "offline"
	// Готовность наступает позже, чем отведено простою.
	base := f.run
	stub := func(name string, args ...string) (string, error) {
		if f.now-start > 120 {
			f.devices[15556] = "device"
		}
		return base(name, args...)
	}
	ctx = ctxOf(c, stub)

	since := time.Unix(start, 0)
	if got := waitReady(ctx, 15556, since, since.Add(c.BootTimeout), since.Add(c.AcquireWait)); got != bootReady {
		t.Fatalf("ожидалась готовность, получено %v", got)
	}
	e, _ := readEntry(ctx, 15556)
	if got := idle(ctx, e); got > c.IdleTimeout {
		t.Errorf("простой ожидавшей сессии %s, ожидалось не больше %s", got, c.IdleTimeout)
	}
	sweepIdle(ctx)
	if got := entries(ctx); len(got) != 1 {
		t.Fatalf("запись, готовности которой ждали, убрана: %v", got)
	}
}

func TestAcquireMarksActivity(t *testing.T) {
	c, f := newTestConfig(t)
	ctx := ctxOf(c, f.run)
	seed(t, c, f, 15556, entry{Session: "s1", CWD: "/w"})
	f.now += 100

	if _, err := acquire(ctx, "s1", 0, "/w"); err != nil {
		t.Fatal(err)
	}
	if e, _ := readEntry(ctx, 15556); e.LastSeen != f.now {
		t.Errorf("повторная выдача не отметила запись: %d, ожидалось %d", e.LastSeen, f.now)
	}
}

// Место, которое держала брошенная сессия, достаётся новой.
func TestIdleEntryFreesSlotForNewSession(t *testing.T) {
	c, f := newTestConfig(t)
	c.Max = 1
	ctx := ctxOf(c, f.run)
	f.starts[100] = "a"
	seed(t, c, f, 15556, entry{Session: "old", OwnerPID: 100, OwnerStart: "a", Since: f.now - 1000, LastSeen: f.now - 1000})

	got, err := acquire(ctx, "new", 0, "/w")
	if err != nil {
		t.Fatalf("выдача не состоялась: %v", err)
	}
	if len(f.spawns) != 1 {
		t.Fatalf("запусков %v, ожидался один", f.spawns)
	}
	ports := entries(ctx)
	if len(ports) != 1 {
		t.Fatalf("в реестре %v, ожидалась одна запись", ports)
	}
	if e, _ := readEntry(ctx, ports[0]); e.Session != "new" {
		t.Errorf("место досталось сессии %q, ожидалась new (выдан %s)", e.Session, got)
	}
}

// Хук приходит из любого проекта, поэтому за брошенной сессией подметает любая
// живая: сама брошенная событий уже не порождает.
func TestHookSweepsIdleRegardlessOfSettings(t *testing.T) {
	for _, tc := range []struct {
		name     string
		body     string
		wantCode int
	}{
		{"файла настроек нет", "", 0},
		{"файл настроек негоден", `{"avd": "img", "typo": 1}`, 2},
	} {
		c, f := newTestConfig(t)
		ctx := ctxOf(c, f.run)
		dir := t.TempDir()
		if tc.body != "" {
			if err := os.WriteFile(filepath.Join(dir, ".git"), []byte("gitdir: /elsewhere\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			writeSettings(t, dir, tc.body)
		}
		f.starts[100] = "a"
		seed(t, c, f, 15556, entry{Session: "old", OwnerPID: 100, OwnerStart: "a", Since: f.now - 1000, LastSeen: f.now - 1000})

		code, out, _ := runHook(t, c, f, bashJSON(t, "s2", dir, "adb install app.apk"))

		if code != tc.wantCode {
			t.Errorf("%s: код=%d, ожидался %d (out=%q)", tc.name, code, tc.wantCode, out)
		}
		if tc.wantCode == 0 && out != "" {
			t.Errorf("%s: непричастный проект должен молчать: %q", tc.name, out)
		}
		if got := entries(ctx); len(got) != 0 {
			t.Errorf("%s: простаивающая запись не убрана: %v", tc.name, got)
		}
	}
}

// Работа сессии это работа, даже когда до устройства она не дотягивается:
// читающая проверка и запрещённая команда отмечают активность наравне с выдачей.
func TestEventsThatServeNoInstanceStillMarkActivity(t *testing.T) {
	for _, tc := range []struct{ name, cmd string }{
		{"читающая проверка", "adb devices"},
		{"запрещённая команда", "adb root"},
		{"нераспознанная команда", "git status"},
	} {
		c, f := newTestConfig(t)
		ctx := ctxOf(c, f.run)
		dir := worktreeDir(t)
		seed(t, c, f, 15556, entry{Session: "s1", CWD: dir})
		f.now += 100

		if code, _, errOut := runHook(t, c, f, bashJSON(t, "s1", dir, tc.cmd)); code != 0 {
			t.Fatalf("%s: код=%d stderr=%q", tc.name, code, errOut)
		}
		if e, _ := readEntry(ctx, 15556); e.LastSeen != f.now {
			t.Errorf("%s: активность не отмечена: %d, ожидалось %d", tc.name, e.LastSeen, f.now)
		}
		if len(f.spawns) != 0 {
			t.Errorf("%s: инстанс не должен запускаться: %v", tc.name, f.spawns)
		}
	}
}

// --- журнал решений ---

func TestLogRecordsLaunchAndDenial(t *testing.T) {
	c, f := newTestConfig(t)
	c.Max = 1
	ctx := ctxOf(c, f.run)
	// Время запуска берётся до выдачи: ожидание готовности двигает часы.
	stamp := time.Unix(f.now, 0).Format(time.RFC3339)

	if _, err := acquire(ctx, "s1", 0, "/w"); err != nil {
		t.Fatal(err)
	}
	// Предел исчерпан: вторая сессия получает отказ.
	if _, err := acquire(ctx, "s2", 0, "/w2"); err == nil {
		t.Fatal("ожидался отказ по пределу")
	}

	got := routerLog(t, c)
	for _, want := range []string{
		stamp + " emulator-15556 launch session=s1 image=pooled_avd cwd=/w",
		"- deny session=s2 reason=all 1 instances are busy; ",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("журнал %q не содержит %q", got, want)
		}
	}
}

// Журнал это наблюдение за пулом, а не условие его работы.
func TestUnwritableLogDoesNotChangeOutcome(t *testing.T) {
	c, f := newTestConfig(t)
	ctx := ctxOf(c, f.run)
	if err := os.Mkdir(filepath.Join(c.Registry, "router.log"), 0o755); err != nil {
		t.Fatal(err)
	}
	seed(t, c, f, 15556, entry{Session: "s1", CWD: "/w"})

	if err := release(ctx, "s1", 0); err != nil {
		t.Fatalf("освобождение должно пройти и без журнала: %v", err)
	}
	if got := entries(ctx); len(got) != 0 {
		t.Errorf("в реестре %v, ожидалась пустота", got)
	}
}
