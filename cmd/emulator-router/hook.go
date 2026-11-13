package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"maps"
	"os"
	"regexp"
	"strconv"
	"strings"
)

type hookInput struct {
	HookEventName string         `json:"hook_event_name"`
	SessionID     string         `json:"session_id"`
	CWD           string         `json:"cwd"`
	ToolName      string         `json:"tool_name"`
	ToolInput     map[string]any `json:"tool_input"`
}

type hookSpecific struct {
	HookEventName            string         `json:"hookEventName,omitempty"`
	PermissionDecision       string         `json:"permissionDecision,omitempty"`
	PermissionDecisionReason string         `json:"permissionDecisionReason,omitempty"`
	UpdatedInput             map[string]any `json:"updatedInput,omitempty"`
	AdditionalContext        string         `json:"additionalContext,omitempty"`
}

type hookOut struct {
	HookSpecificOutput hookSpecific `json:"hookSpecificOutput"`
}

// Ядро распознавания: обращения, общие для любого проекта с Android-эмулятором.
// Обёртки конкретного проекта перечисляются в его настройках (поле reach) и
// сопоставляются отдельно, с тем же требованием позиции команды. Набор заведомо
// неполон: ложноотрицательное срабатывание — принятый компромисс, а не дефект.
var (
	probeRe  = regexp.MustCompile(`^[[:space:]]*adb[[:space:]]+(devices([[:space:]]+-l)?|version|start-server)[[:space:]]*$`)
	reachRe  = regexp.MustCompile(commandPos + `(adb|emulator)[[:space:]]|gradlew.*(install|connected)`)
	rootRe   = regexp.MustCompile(cmdStart + `([^[:space:]]*/)?adb([[:space:]]+-s[[:space:]]+[^[:space:]]+)?[[:space:]]+root([[:space:]]|$)`)
	launchRe = regexp.MustCompile(cmdStart + `((nohup|bash|sh|zsh)[[:space:]]+)?([^[:space:]]*/)?emulator[[:space:]]`)
	targetRe = regexp.MustCompile(`([^[:space:]]*adb)([[:space:]]+)-s[[:space:]]+[^[:space:]]+`)
	anyEmuRe = regexp.MustCompile(`([^[:space:]]*adb)([[:space:]]+)-e([[:space:]])`)
)

// reaches: ядро либо любая из обёрток, перечисленных в настройках проекта.
func reaches(c config, cmd string) bool {
	if reachRe.MatchString(cmd) {
		return true
	}
	for _, re := range c.Reach {
		if re.MatchString(cmd) {
			return true
		}
	}
	return false
}

func hook(ctx context.Context, stdin io.Reader, stdout, stderr io.Writer) int {
	raw, err := io.ReadAll(stdin)
	if err != nil {
		fmt.Fprintln(stderr, "hook: cannot read stdin:", err)
		return 2
	}
	var in hookInput
	if err := json.Unmarshal(raw, &in); err != nil {
		fmt.Fprintln(stderr, "hook: unparsable input:", err)
		return 2
	}

	// Отметка активности и уборка по простою — до разбора настроек и до всех
	// условий, при которых система в вызов не вмешивается. Работа сессии это
	// работа, даже когда до устройства она не дотягивается, а уборке ни настройки,
	// ни имя образа не требуются — реестр один на машину. Хук приходит из любого
	// проекта, поэтому подметает за брошенными сессиями любая живая: сама
	// брошенная сессия событий уже не порождает.
	if in.HookEventName == "PreToolUse" || in.HookEventName == "SessionEnd" {
		touch(ctx, in.SessionID)
		sweepIdle(ctx)
	}

	switch in.HookEventName {
	case "SessionEnd":
		// Освобождение безусловно: записи, сделанные до правила о рабочем
		// каталоге, освобождаются так же, как все остальные. Блокировать здесь
		// нечего, поэтому ошибка это код 1.
		if in.SessionID == "" {
			fmt.Fprintln(stderr, "hook: SessionEnd without session_id")
			return 1
		}
		if err := release(ctx, in.SessionID, ownerPID(ctx)); err != nil {
			fmt.Fprintln(stderr, "hook:", err)
			return 1
		}
		return 0
	case "PreToolUse":
	default:
		return 0 // не наше событие
	}

	// Код, при котором вызов инструмента исполняется, для внутренних ошибок не
	// используется: вызов без привязки ушёл бы на произвольное устройство.
	if in.SessionID == "" {
		fmt.Fprintln(stderr, "hook: PreToolUse without session_id")
		return 2
	}
	cwd := in.CWD
	if cwd == "" {
		cwd, _ = os.Getwd()
	}
	path, mainCheckout := findSettings(cwd)
	if path == "" {
		return 0 // проект не настроен на пул: устройство остаётся делом пользователя
	}
	c, err := applySettings(cfg(ctx), path)
	if err != nil {
		// Найденный файл означает сознательную настройку, поэтому продолжать на
		// дефолтах нельзя: вызов ушёл бы на инстанс не того образа.
		fmt.Fprintln(stderr, "hook:", err)
		return 2
	}
	ctx = cfgKey.With(ctx, c)
	if mainCheckout && !c.MainCheckout {
		return 0 // в главном чекауте устройство остаётся делом пользователя
	}

	switch {
	case in.ToolName == "Bash":
		return bashCall(ctx, in, cwd, stdout, stderr)
	case in.ToolName == "mcp__mobile-mcp__mobile_list_available_devices":
		return 0 // нечего подменять, и перечисление не должно стоить загрузки
	case strings.HasPrefix(in.ToolName, "mcp__mobile-mcp__"):
		serial, err := acquire(ctx, in.SessionID, ownerPID(ctx), cwd)
		if err != nil {
			return deny(stdout, "No emulator for this session: "+err.Error())
		}
		return emit(stdout, hookSpecific{UpdatedInput: withOverride(in.ToolInput, "device", serial)})
	}
	return 0 // неизвестный инструмент это неверный matcher, а не отказ пула
}

func bashCall(ctx context.Context, in hookInput, cwd string, stdout, stderr io.Writer) int {
	c := cfg(ctx)
	cmd, _ := in.ToolInput["command"].(string)
	if cmd == "" {
		fmt.Fprintln(stderr, "hook: PreToolUse for Bash without tool_input.command")
		return 2
	}
	if probeRe.MatchString(cmd) {
		return 0 // читающая проверка не должна стоить сессии загрузки
	}
	if !reaches(c, cmd) {
		return 0
	}

	// Запреты проверяются до обращения за инстансом: запрещённый вызов не должен
	// приводить к запуску эмулятора.
	if rootRe.MatchString(cmd) {
		return deny(stdout, "Do not run 'adb root' here: it restarts adbd and takes every pooled emulator offline, including other sessions'. If the image allows it, run the single command as root instead: adb shell su 0 <command>.")
	}
	avdRe := regexp.MustCompile(`(@|-avd[[:space:]]+)` + regexp.QuoteMeta(c.AVD) + `([[:space:]]|$)`)
	if launchRe.MatchString(cmd) && avdRe.MatchString(cmd) && !strings.Contains(cmd, "-read-only") {
		return deny(stdout, fmt.Sprintf("Do not launch %q without -read-only: that destroys its default_boot snapshot and every pooled instance cold boots afterwards. Regenerate the snapshot from the main checkout instead.", c.AVD))
	}

	serial, err := acquire(ctx, in.SessionID, ownerPID(ctx), cwd)
	if err != nil {
		return deny(stdout, "No emulator for this session: "+err.Error())
	}

	// Явный -s побеждает ANDROID_SERIAL, поэтому серийник, скопированный из
	// перечисления устройств, дотянулся бы до чужого инстанса. Нормализуются все
	// вхождения; -e тоже, при нескольких инстансах оно неоднозначно.
	cmd = targetRe.ReplaceAllString(cmd, "${1}${2}-s "+serial)
	cmd = anyEmuRe.ReplaceAllString(cmd, "${1}${2}-s "+serial+"${3}")

	// `export ...; cmd`, а не `VAR=x cmd`: префикс привязывается только к первой
	// простой команде, поэтому в `cd app && adb install ...` до adb не дойдёт.
	return emit(stdout, hookSpecific{
		UpdatedInput:      withOverride(in.ToolInput, "command", "export ANDROID_SERIAL="+serial+" ADB_SERIAL="+serial+"; "+cmd),
		AdditionalContext: "This session owns emulator " + serial + ". Use it for every device call.",
	})
}

// withOverride: изменённый ввод заменяет ввод вызова целиком, а не сливается с
// ним по полям, поэтому непереопределённые аргументы переносятся сюда сами. Без
// них вызов негоден по схеме своего инструмента: у Mobile-MCP пропали бы
// обязательные аргументы, у оболочки — признак фонового исполнения и предел
// времени. Копия, а не правка на месте: разобранный ввод остаётся тем, что пришло.
func withOverride(in map[string]any, key string, value any) map[string]any {
	out := make(map[string]any, len(in)+1)
	maps.Copy(out, in)
	out[key] = value
	return out
}

// emit: имя события обязательно в любом решении, а не только в отказе. Claude
// Code разбирает hookSpecificOutput размеченным объединением по этому полю, и без
// него решение молча отбрасывается целиком — вызов уходит неизменённым, то есть
// на произвольное устройство. Проставляется здесь, потому что через emit проходят
// все выводы.
func emit(w io.Writer, s hookSpecific) int {
	s.HookEventName = "PreToolUse"
	b, err := json.Marshal(hookOut{HookSpecificOutput: s})
	if err != nil {
		return 2
	}
	fmt.Fprintln(w, string(b))
	return 0
}

func deny(w io.Writer, reason string) int {
	return emit(w, hookSpecific{
		PermissionDecision:       "deny",
		PermissionDecisionReason: reason,
	})
}

// ownerPID: собственный родитель это одноразовая оболочка, поэтому владельцем
// считается ближайший предок-процесс сессии. Его pid живёт ровно столько, сколько
// сессия, и субагенты разделяют его, сообщая собственный session_id.
func ownerPID(ctx context.Context) int {
	pid := os.Getpid()
	for range 8 {
		out, err := run(ctx, "ps", "-o", "ppid=", "-p", strconv.Itoa(pid))
		if err != nil {
			return 0
		}
		p, err := strconv.Atoi(strings.TrimSpace(out))
		if err != nil || p <= 1 {
			return 0
		}
		pid = p
		if comm, err := run(ctx, "ps", "-o", "comm=", "-p", strconv.Itoa(pid)); err == nil && strings.Contains(comm, "claude") {
			return pid
		}
	}
	return 0
}
