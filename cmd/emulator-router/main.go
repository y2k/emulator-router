// Пул Android-эмуляторов: каждой сессии Claude Code свой инстанс `emulator -read-only`.
// Один бинарь совмещает роутинг вызовов хука и управление пулом.
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/y2k/emulator-router/internal/ctxkey"
)

// config — настройки со швами во внешний мир. Часть полей приходит из файла
// настроек проекта (см. settings), остальные зашиты: расположение реестра,
// диапазон портов и пределы времени различаются не между проектами, а между
// реализациями, поэтому настройками проекта не являются.
type config struct {
	AVD string
	// ponytail: предел общий на машину, а задаётся проектом, поэтому при двух
	// одновременных проектах работает как «кто первый занял». Разделять реестры
	// по проектам — если это начнёт мешать; сейчас цена выше пользы, потому что
	// уборка не трогает инстансы вне своего реестра.
	Max int
	GPU string
	// Расход диска на инстанс: потребитель один — doctor. Замерено на инстансе
	// пула, с запасом на сборку и IDE рядом.
	DiskPerInstanceGB int
	Reach             []*regexp.Regexp
	MainCheckout      bool
	Registry          string
	COWDir            string
	SDK               string
	PortMin           int
	PortMax           int
	BootTimeout       time.Duration
	OfflineTimeout    time.Duration
	// AcquireWait — предел на всю выдачу целиком, а не на каждое ожидание внутри
	// неё: складываются именно они. Заведомо меньше 600 секунд — предела, который
	// вызывающая сторона отводит команде хука по умолчанию, — потому что
	// прерванная по этому пределу обработка вызов не блокирует: он исполняется
	// без привязки и уходит на произвольное устройство. Не настройка проекта:
	// пределы времени различаются между реализациями, а не между проектами.
	AcquireWait time.Duration
	// ProcessGrace — запас, в течение которого отсутствие процесса инстанса не
	// считается его гибелью. Программа эмулятора запускает qemu не мгновенно, и
	// без запаса каждый запуск обрывался бы на первой же проверке.
	ProcessGrace time.Duration

	// ponytail: четыре функции вместо перечисленного интерфейса из design.md.
	// Тестируемость даёт шов, а не длина его описания; расширять до интерфейса,
	// если появится вторая продовая реализация. Пятый шов — запуск внешней
	// команды — уехал отдельным ключом контекста (см. runner).
	Spawn  func(logPath, name string, args ...string) error
	Signal func(pid int, sig syscall.Signal) error
	Now    func() time.Time
	Sleep  func(time.Duration)
}

func defaultConfig() config {
	home, _ := os.UserHomeDir()
	return config{
		Max:               3,
		GPU:               "host",
		DiskPerInstanceGB: 6,
		Registry:          filepath.Join(home, ".claude", "emulator-router"),
		// Путь bash-версии: именно здесь эмулятор пишет COW-слои.
		COWDir:         "/tmp/android-" + os.Getenv("USER"),
		SDK:            sdkPath(home),
		PortMin:        5556,
		PortMax:        5682,
		BootTimeout:    300 * time.Second,
		OfflineTimeout: 30 * time.Second,
		AcquireWait:    480 * time.Second,
		ProcessGrace:   15 * time.Second,
		Spawn:          spawn,
		Signal:         syscall.Kill,
		Now:            time.Now,
		Sleep:          time.Sleep,
	}
}

// sdkPath: расположение SDK — свойство машины, а не проекта, поэтому берётся из
// окружения, а не из настроек. Стандартные расположения у macOS и Linux разные.
func sdkPath(home string) string {
	for _, env := range []string{"ANDROID_HOME", "ANDROID_SDK_ROOT"} {
		if v := os.Getenv(env); v != "" {
			return v
		}
	}
	if runtime.GOOS == "darwin" {
		return filepath.Join(home, "Library", "Android", "sdk")
	}
	return filepath.Join(home, "Android", "Sdk")
}

// settingsFile — имя файла настроек проекта. Рядом с настройками хука, которые
// при установке всё равно приходится править.
var settingsFile = filepath.Join(".claude", "emulator-router.json")

// settings — настройки проекта. Здесь только то, что различается между
// проектами: предел инстансов и ускоритель графики свойства машины и образа, а
// не проекта, и настройками не являются — заданные, они делают файл негодным как
// незнакомые поля.
//
// Указатель у необязательного поля отличает «не задано» от заданного нулевого
// значения: `"mainCheckout": false` — заданное значение, совпадающее с
// умолчанием, а не молчаливый возврат к нему.
type settings struct {
	AVD          string   `json:"avd"`
	Reach        []string `json:"reach"`
	MainCheckout *bool    `json:"mainCheckout"`
}

// findSettings идёт вверх от cwd, отвечая за один проход на два вопроса: где
// файл настроек проекта и является ли каталог главным чекаутом. У связанного
// рабочего дерева `.git` это файл-ссылка, у главного чекаута — каталог. Каталог
// вне репозитория главным чекаутом не считается.
//
// Обход останавливается на границе репозитория, чтобы настройки постороннего
// родительского каталога не утекли в проект. Связанное рабочее дерево при этом
// берёт настройки из главного чекаута: они принадлежат репозиторию, а не
// конкретному чекауту, поэтому копия в каждом дереве не нужна — и её отсутствие
// не должно молча отключать пул.
func findSettings(cwd string) (path string, mainCheckout bool) {
	for dir := cwd; ; {
		if path == "" {
			if p := filepath.Join(dir, settingsFile); fileExists(p) {
				path = p
			}
		}
		if fi, err := os.Stat(filepath.Join(dir, ".git")); err == nil {
			if path == "" && !fi.IsDir() {
				path = repoSettings(filepath.Join(dir, ".git"))
			}
			return path, fi.IsDir()
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return path, false
		}
		dir = parent
	}
}

// repoSettings достаёт настройки главного чекаута по файлу-ссылке `.git`
// связанного рабочего дерева. Ссылка указывает внутрь каталога git главного
// чекаута (`<чекаут>/.git/worktrees/<имя>`), поэтому чекаут — это родитель
// элемента `.git` в этом пути. Разбор строки вместо вызова git: git как
// зависимость времени исполнения того не стоит.
func repoSettings(gitFile string) string {
	b, err := os.ReadFile(gitFile)
	if err != nil {
		return ""
	}
	gitdir, ok := strings.CutPrefix(strings.TrimSpace(string(b)), "gitdir:")
	if !ok {
		return ""
	}
	gitdir = strings.TrimSpace(gitdir)
	if !filepath.IsAbs(gitdir) {
		gitdir = filepath.Join(filepath.Dir(gitFile), gitdir)
	}
	for dir := filepath.Clean(gitdir); ; {
		parent := filepath.Dir(dir)
		if parent == dir {
			return ""
		}
		if filepath.Base(dir) == ".git" {
			if p := filepath.Join(parent, settingsFile); fileExists(p) {
				return p
			}
			return ""
		}
		dir = parent
	}
}

func fileExists(path string) bool {
	fi, err := os.Stat(path)
	return err == nil && !fi.IsDir()
}

// commandPos — требование позиции команды: в начале строки либо после
// разделителя, с необязательной обёрткой и путём. Навешивается и на ядро
// распознавания, и на каждый шаблон из настроек, поэтому гарантия «упоминание
// слова внутри кавычек не занимает инстанс» не зависит от аккуратности того,
// кто настройки писал.
const commandPos = `(^|[;&|(){}]|&&|\|\|)[[:space:]]*((nohup|bash|sh|zsh|kotlin|kotlinc)[[:space:]]+)?([^[:space:]]*/)?`

// applySettings дополняет дефолты файлом настроек. Разбор строгий: незнакомое
// поле — ошибка, иначе опечатка в имени неотличима от работающей настройки.
// Регулярки компилируются здесь же, а не при первом сопоставлении: негодный
// шаблон должен называться сразу, а не при команде, которая до него дотянулась.
func applySettings(c config, path string) (config, error) {
	f, err := os.Open(path)
	if err != nil {
		return c, err
	}
	defer f.Close()

	var s settings
	dec := json.NewDecoder(f)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&s); err != nil {
		return c, fmt.Errorf("%s: %w", path, err)
	}
	if s.AVD == "" {
		return c, fmt.Errorf(`%s: field "avd" is required: there is no default AVD name`, path)
	}

	c.AVD = s.AVD
	if s.MainCheckout != nil {
		c.MainCheckout = *s.MainCheckout
	}
	for _, pat := range s.Reach {
		re, err := regexp.Compile(commandPos + `(?:` + pat + `)`)
		if err != nil {
			return c, fmt.Errorf(`%s: field "reach" has an invalid pattern %q: %w`, path, pat, err)
		}
		c.Reach = append(c.Reach, re)
	}
	return c, nil
}

// Настройки передаются контекстом, а не приёмником: значение кладётся один раз в
// main и достаётся хелпером cfg там, где нужно.
var cfgKey = ctxkey.New[config]("config")

func cfg(ctx context.Context) config { return cfgKey.MustGet(ctx) }

// runner — эффект «запустить внешнюю команду». Едет собственным ключом, а не
// полем настроек: настройки различаются между проектами, а шов во внешний мир —
// между продом и тестом, и функции, которой нужен только запуск команды,
// незачем объявлять зависимость от всех настроек сразу.
type runner func(name string, args ...string) (string, error)

var runKey = ctxkey.New[runner]("run")

// run — хелпер поверх ключа, зеркало cfg. MustGet, а не откат на execRun:
// забытая подстановка в тесте должна падать с именем ключа, а не запускать
// настоящие ps и adb на машине разработчика.
func run(ctx context.Context, name string, args ...string) (string, error) {
	return runKey.MustGet(ctx)(name, args...)
}

func adb(ctx context.Context) string {
	return filepath.Join(cfg(ctx).SDK, "platform-tools", "adb")
}

func emu(ctx context.Context) string {
	return filepath.Join(cfg(ctx).SDK, "emulator", "emulator")
}

func execRun(name string, args ...string) (string, error) {
	out, err := exec.Command(name, args...).Output()
	return string(out), err
}

// spawn запускает инстанс отдельной сессией процессов, чтобы он пережил хук.
func spawn(logPath, name string, args ...string) error {
	f, err := os.Create(logPath)
	if err != nil {
		return err
	}
	defer f.Close()
	cmd := exec.Command(name, args...)
	cmd.Stdout, cmd.Stderr = f, f
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		return err
	}
	go cmd.Wait()
	return nil
}

func main() {
	ctx := runKey.With(cfgKey.With(context.Background(), defaultConfig()), execRun)
	os.Exit(dispatch(ctx, os.Args[1:], os.Stdin, os.Stdout, os.Stderr))
}

func dispatch(ctx context.Context, args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	cmd := "status"
	if len(args) > 0 {
		cmd, args = args[0], args[1:]
	}
	arg := func(i int) string {
		if i < len(args) {
			return args[i]
		}
		return ""
	}
	owner := func() int { n, _ := strconv.Atoi(arg(1)); return n }

	switch cmd {
	case "hook":
		return hook(ctx, stdin, stdout, stderr)
	case "acquire":
		cwd := arg(2)
		if cwd == "" {
			cwd, _ = os.Getwd()
		}
		// Здесь, в отличие от хука, ненайденный файл — ошибка: подкоманду
		// запускают руками и именно для этого каталога.
		path, _ := findSettings(cwd)
		if path == "" {
			fmt.Fprintf(stderr, "no %s found above %s\n", settingsFile, cwd)
			return 1
		}
		c, err := applySettings(cfg(ctx), path)
		if err != nil {
			fmt.Fprintln(stderr, err)
			return 1
		}
		serial, err := acquire(cfgKey.With(ctx, c), arg(0), owner(), cwd)
		if err != nil {
			fmt.Fprintln(stderr, err)
			return 1
		}
		fmt.Fprintln(stdout, serial)
	case "release":
		if err := release(ctx, arg(0), owner()); err != nil {
			fmt.Fprintln(stderr, err)
			return 1
		}
	case "doctor":
		cwd := arg(0)
		if cwd == "" {
			cwd, _ = os.Getwd()
		}
		return doctor(ctx, cwd, stdout)
	case "reap":
		reap(ctx)
	case "status":
		fmt.Fprint(stdout, statusText(ctx))
	default:
		fmt.Fprintln(stderr, "usage: emulator-router {hook|acquire <session> [owner] [cwd]|release <session> [owner]|doctor [cwd]|reap|status}")
		return 1
	}
	return 0
}

// version заполняется при сборке (см. Makefile). Значение по умолчанию явное:
// собранный без версии бинарь тоже надо уметь опознать в отчёте doctor.
var version = "unknown"
