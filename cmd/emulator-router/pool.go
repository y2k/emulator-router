package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
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
	// Image — образ, из которого инстанс поднят. Реестр один на машину, а образ
	// приходит из настроек проекта, поэтому без него инстанс прежнего образа
	// выдавался бы как свой после обновления образа в настройках.
	Image string `json:"image"`
	// LastSeen — время последней активности сессии, владеющей инстансом. Это
	// единственный признак, отличающий брошенную сессию от работающей: владелец
	// брошенной жив, процесс её инстанса жив, а у записи с неизвестным владельцем
	// о смерти владельца судить не по чему вовсе.
	//
	// Отсутствующее значение (записи прежних версий) особого случая не требует:
	// нулевое время — это 1970 год, то есть простой длиной в полвека, а такая
	// запись и должна уходить первой.
	LastSeen int64 `json:"last_seen"`
}

var (
	digits = regexp.MustCompile(`^[0-9]+$`)
	spaces = regexp.MustCompile(` +`)
)

// found — исход поиска инстанса сессии. Загружающийся инстанс отличается и от
// готового, и от отсутствующего: выдать его нельзя, а утилизировать нельзя тем
// более — это начало бы загрузку заново.
type found int

const (
	foundNone found = iota
	foundReady
	foundBooting
)

// boot — исход ожидания готовности. bootPending означает, что раньше готовности
// кончился бюджет выдачи: инстанс продолжает загружаться, и его подберёт
// следующее обращение. bootFailed — ждать больше нечего, инстанс утилизируется.
type boot int

const (
	bootReady boot = iota
	bootPending
	bootFailed
)

func entryPath(ctx context.Context, port int) string {
	return filepath.Join(cfg(ctx).Registry, strconv.Itoa(port))
}
func logPath(ctx context.Context, port int) string { return entryPath(ctx, port) + ".log" }

// logDecision дозаписывает строку в журнал решений, меняющих состав пула:
// запуск, отказ, завершение. Уборка по простою наблюдаема только так — она
// происходит в чужом процессе, между сессиями, и по исчезнувшей записи отличить
// её от других видов уборки нечем.
//
// Дозапись, а не открытый на всё время файл: писателей столько же, сколько
// вызовов инструмента, и каждый — отдельный процесс. Короткая строка при O_APPEND
// не рвётся. Время из того же шва, что и остальные часы, поэтому в тестах оно
// предсказуемо. Ошибка записи игнорируется: пул работает и без наблюдения за ним.
//
// Отметка активности здесь не появляется: она происходит на каждом вызове
// инструмента и вытеснила бы из журнала всё остальное.
//
// ponytail: без ротации — строка на решение это десятки строк в день. Вернуться,
// если файл начнёт мешать.
func logDecision(ctx context.Context, format string, args ...any) {
	c := cfg(ctx)
	f, err := os.OpenFile(journalPath(ctx), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return
	}
	defer f.Close()
	fmt.Fprintf(f, "%s %s\n", c.Now().Format(time.RFC3339), fmt.Sprintf(format, args...))
}

func journalPath(ctx context.Context) string {
	return filepath.Join(cfg(ctx).Registry, "router.log")
}

// journalSince — хвост журнала, начиная с первой записи не старше окна. Хвост, а
// не отбор записей по одной: строку, чьё время не разобралось, отбор потерял бы
// молча, а журнал читают именно затем, чтобы понять, что произошло. Отсутствие
// журнала — то же, что пустое окно: до первого решения файла нет.
//
// ponytail: писатели — разные процессы, поэтому запись, отставшая от границы
// окна, попадёт в вывод вместе с хвостом. Лишняя строка дешевле потерянной;
// вернуться к отбору по одной, если начнёт мешать.
func journalSince(ctx context.Context, window time.Duration) string {
	b, err := os.ReadFile(journalPath(ctx))
	if err != nil {
		return ""
	}
	since := cfg(ctx).Now().Add(-window)
	lines := strings.SplitAfter(string(b), "\n")
	for i, line := range lines {
		stamp, _, _ := strings.Cut(strings.TrimSpace(line), " ")
		at, err := time.Parse(time.RFC3339, stamp)
		if err != nil || at.Before(since) {
			continue
		}
		return strings.Join(lines[i:], "")
	}
	return ""
}

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
	// Запись появляется в реестре целиком: перезапись на месте проходит через
	// усечённое состояние, а усечённая запись неотличима от нечитаемой — держит
	// порт и место в пределе, а сессии не достаётся. Временный файл лежит в том
	// же каталоге, иначе замена не была бы переименованием.
	tmp, err := os.CreateTemp(c.Registry, strconv.Itoa(port)+".*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(b); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Chmod(tmp.Name(), 0o644); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), entryPath(ctx, port))
}

func dropEntry(ctx context.Context, port int) {
	os.Remove(entryPath(ctx, port))
	os.Remove(logPath(ctx, port))
}

// idle — простой записи: сколько прошло с последней активности её сессии.
func idle(ctx context.Context, e entry) time.Duration {
	return cfg(ctx).Now().Sub(time.Unix(e.LastSeen, 0))
}

// idleText — простой для человека. Запись без отметки это «never», а не полвека:
// первое же завершение после обновления придётся как раз на такую запись, и
// шестизначное число часов в журнале выглядело бы переполнением, а не причиной.
func idleText(ctx context.Context, e entry) string {
	if e.LastSeen == 0 {
		return "never"
	}
	return idle(ctx, e).Round(time.Second).String()
}

// touchPort отмечает активность в записи порта. Ошибки игнорируются: отметка это
// наблюдение за пулом, а не условие его работы, и запись могли удалить прямо
// сейчас.
func touchPort(ctx context.Context, port int) {
	if e, ok := readEntry(ctx, port); ok {
		e.LastSeen = cfg(ctx).Now().Unix()
		writeEntry(ctx, port, e)
	}
}

// touch отмечает активность сессии в её записи. Опознание идёт по одному
// идентификатору сессии, без владельца: определение владельца стоит обхода
// предков процесса, а отметка ставится на каждом вызове инструмента в любом
// проекте машины — цена этого обхода там несоизмерима с пользой. Инстанс,
// записанный на другую сессию того же владельца (так бывает у субагента),
// отмечается своими обращениями к устройству, поскольку выдача владельца
// учитывает.
func touch(ctx context.Context, session string) {
	if session == "" {
		return
	}
	for _, port := range entries(ctx) {
		if e, ok := readEntry(ctx, port); ok && e.Session == session {
			e.LastSeen = cfg(ctx).Now().Unix()
			writeEntry(ctx, port, e)
			return
		}
	}
}

// procStart — время запуска процесса. Номер процесса сам по себе не
// идентификатор: система его переиспользует.
func procStart(ctx context.Context, pid int) string {
	out, err := run(ctx, "ps", "-o", "lstart=", "-p", strconv.Itoa(pid))
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
	out, err := run(ctx, adb(ctx), "devices")
	if err != nil {
		return ""
	}
	want := serial(port)
	for line := range strings.SplitSeq(out, "\n") {
		f := strings.Fields(line)
		if len(f) >= 2 && f[0] == want {
			return f[1]
		}
	}
	return ""
}

func serial(port int) string { return "emulator-" + strconv.Itoa(port) }

func qemuPID(ctx context.Context, port int) (int, bool) {
	out, err := run(ctx, "pgrep", "-f", fmt.Sprintf("qemu-system.*-port %d( |$)", port))
	if err != nil {
		return 0, false
	}
	for line := range strings.SplitSeq(out, "\n") {
		if pid, err := strconv.Atoi(strings.TrimSpace(line)); err == nil && pid > 0 {
			return pid, true
		}
	}
	return 0, false
}

// killInstance завершает инстанс. Причина приходит аргументом, а не выводится
// из места вызова: путей завершения шесть, и журналу нужна причина каждого, а
// одна точка записи дешевле шести.
func killInstance(ctx context.Context, port int, reason string) {
	c := cfg(ctx)
	logDecision(ctx, "%s kill %s", serial(port), reason)
	run(ctx, adb(ctx), "-s", serial(port), "emu", "kill")
	if pid, ok := qemuPID(ctx, port); ok {
		c.Sleep(2 * time.Second)
		if c.Signal(pid, 0) == nil {
			c.Signal(pid, syscall.SIGKILL)
		}
	}
}

// sweepIdle убирает записи простаивающих сессий — пятый вид мусора и
// единственный, определяемый не гибелью, а временем: брошенная сессия неотличима
// от работающей ни по владельцу, ни по процессу инстанса.
//
// Отдельный проход, а не шаг reap, именно из-за цены: здесь читаются несколько
// небольших файлов и сравниваются числа, а внешние команды запускаются только
// когда инстанс действительно завершается. Только поэтому проход можно выполнять
// на каждом вызове инструмента в любом проекте машины; внутри reap он унаследовал
// бы pgrep и перечисление устройств.
func sweepIdle(ctx context.Context) {
	c := cfg(ctx)
	for _, port := range entries(ctx) {
		e, ok := readEntry(ctx, port)
		if !ok {
			continue // нечитаемую запись разбирает reap: простоя у неё нет
		}
		// Запись создаётся до запуска, а загрузка событий хука не порождает:
		// свежая запись это не простой, а запуск, который может быть не завершён.
		if c.Now().Unix()-e.Since <= int64(c.BootTimeout.Seconds()) {
			continue
		}
		if idle(ctx, e) <= c.IdleTimeout {
			continue
		}
		// Запись перечитывается перед завершением: сессия могла отметиться, пока
		// шёл проход. Окно остаётся, но сокращается до микросекунд, а проигравший
		// в нём получает новый инстанс на общих условиях.
		if e, ok := readEntry(ctx, port); !ok || idle(ctx, e) <= c.IdleTimeout {
			continue
		}
		killInstance(ctx, port, fmt.Sprintf("reason=idle idle=%s session=%s", idleText(ctx, e), e.Session))
		dropEntry(ctx, port)
	}
}

// reap убирает пять видов мусора: записи с мёртвым владельцем, записи с живым
// владельцем и погибшим инстансом, нечитаемые записи, записи простаивающих
// сессий, неиспользуемые COW-слои.
//
// Инстансы, отсутствующие в собственном реестре, не трогаются вовсе: bash-версия
// прибивала все read-only процессы эмулятора на машине, спецификация это запрещает.
func reap(ctx context.Context) {
	c := cfg(ctx)
	sweepIdle(ctx)
	for _, port := range entries(ctx) {
		e, ok := readEntry(ctx, port)
		if !ok {
			// Нечитаемую запись не может использовать ни одна сессия, а место в
			// пределе и порт она держит, поэтому пропуск оставлял бы пул меньше
			// навсегда. Запас отмеряется по времени изменения файла: времени
			// создания у неё нет, а писать её могли прямо сейчас. Инстанс не
			// завершается: чей он, сказать нечем.
			if fi, err := os.Stat(entryPath(ctx, port)); err == nil && c.Now().Sub(fi.ModTime()) > c.BootTimeout {
				dropEntry(ctx, port)
			}
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
			killInstance(ctx, port, "reason=instance-gone session="+e.Session)
			dropEntry(ctx, port)
			continue
		}
		killInstance(ctx, port, fmt.Sprintf("reason=owner-dead owner=%d session=%s", e.OwnerPID, e.Session))
		dropEntry(ctx, port)
	}
	// При принудительном завершении qemu не удаляет свой слой сам.
	//
	// Слой сохраняется не только занятым, но и тогда, когда занятость проверить
	// нечем: отсутствие ответа — не ответ «свободен». Не запустившаяся команда
	// отличается от ответившей типом ошибки: у первой это *exec.Error, у второй —
	// код возврата.
	layers, _ := filepath.Glob(filepath.Join(c.COWDir, "emulator-*.qcow2"))
	for _, f := range layers {
		var notRun *exec.Error
		if _, err := run(ctx, "lsof", "-t", f); err != nil && !errors.As(err, &notRun) {
			os.Remove(f)
		}
	}
}

// mine ищет инстанс сессии или её владельца. deadline ограничивает ожидание;
// нулевое значение означает «не ждать вовсе».
//
// Недоступность при живом процессе означает одно из двух, и различает их возраст
// записи. Запись моложе времени, отведённого на загрузку, — это продолжающаяся
// загрузка, и ждать её нужно до конца этого времени. Запись старше — инстанс,
// который был готов и стал недоступен, почти всегда из-за перезапуска сервера
// отладки; ему отведено время на восстановление, после чего он утилизируется.
func mine(ctx context.Context, session string, owner int, deadline time.Time) (int, found) {
	c := cfg(ctx)
	for _, port := range entries(ctx) {
		e, ok := readEntry(ctx, port)
		if !ok {
			continue
		}
		if e.Session != session && !(owner != 0 && e.OwnerPID == owner) {
			continue
		}
		// Инстанс другого образа не свой: сессия просила один образ, а работала бы
		// с другим молча. Освобождение, а не отказ: сессии нужен рабочий инстанс,
		// и выдача тут же поднимет инстанс запрошенного образа. Освобождение при
		// этом от образа не зависит — оно приходит с настройками по умолчанию, где
		// образа нет, и попадает в эту же ветвь.
		if e.Image != c.AVD {
			killInstance(ctx, port, fmt.Sprintf("reason=image-mismatch was=%s want=%s session=%s", e.Image, c.AVD, e.Session))
			dropEntry(ctx, port)
			continue
		}
		if deviceState(ctx, port) == "device" {
			return port, foundReady
		}
		if _, live := qemuPID(ctx, port); live {
			// Соединение восстанавливается в обоих случаях: свежий инстанс мог
			// не попасть в перечисление, готовый — выпасть из него.
			run(ctx, adb(ctx), "reconnect", "offline")
			since := time.Unix(e.Since, 0)
			until := since.Add(c.BootTimeout)
			if now := c.Now(); now.After(until) {
				until = now.Add(c.OfflineTimeout)
			}
			switch waitReady(ctx, port, since, until, deadline) {
			case bootReady:
				return port, foundReady
			case bootPending:
				return port, foundBooting
			}
		}
		// Запись переживает свой инстанс при аварии процесса или закрытии окна;
		// выдача такого серийника привязала бы сессию к мёртвому устройству.
		killInstance(ctx, port, "reason=unavailable session="+e.Session)
		dropEntry(ctx, port)
	}
	return 0, foundNone
}

func portFree(ctx context.Context, port int) bool {
	if _, err := os.Stat(entryPath(ctx, port)); err == nil {
		return false
	}
	if out, err := run(ctx, adb(ctx), "devices"); err == nil && strings.Contains(out, serial(port)) {
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
// недостижимо. Своего предела у ожидания блокировки нет: он складывался бы с
// остальными, поэтому ожидание ограничено общим бюджетом выдачи.
func lock(ctx context.Context, deadline time.Time) (*os.File, error) {
	c := cfg(ctx)
	if err := os.MkdirAll(c.Registry, 0o755); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(filepath.Join(c.Registry, ".launch.lock"), os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return nil, err
	}
	for {
		if syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB) == nil {
			return f, nil
		}
		if !c.Now().Before(deadline) {
			f.Close()
			return nil, fmt.Errorf("another session is launching an instance, retry in a moment")
		}
		c.Sleep(2 * time.Second)
	}
}

func unlock(f *os.File) {
	syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
	f.Close()
}

// waitReady ждёт готовности инстанса. Загруженный это ещё не пригодный: пока
// свежий клон регистрируется в сервере отладки, обращения к любому инстансу могут
// не выполняться, поэтому готовностью считаются присутствие в перечислении и два
// последовательных подтверждения загрузки.
//
// Пределов два, и различает их именно исход. until — конец времени, отведённого
// на само ожидание: за ним инстанс подлежит утилизации. deadline — конец бюджета
// выдачи: за ним инстанс остаётся жить, а обращение отказывается с предложением
// повторить. since — момент, с которого инстанс существует; он нужен, чтобы
// отсутствие процесса в первые секунды не считалось гибелью.
func waitReady(ctx context.Context, port int, since, until, deadline time.Time) boot {
	c := cfg(ctx)
	streak := 0
	for {
		// Ожидание готовности это активность: событий хука сессия в это время не
		// порождает, а загрузка длится дольше отведённого простою времени. Без
		// отметки здесь уборка из чужого процесса завершила бы инстанс прямо под
		// тем обращением, которое его ждёт.
		touchPort(ctx, port)
		if now := c.Now(); now.After(until) {
			return bootFailed
		} else if now.After(deadline) {
			return bootPending
		} else if _, live := qemuPID(ctx, port); !live && now.Sub(since) > c.ProcessGrace {
			// Процесса нет, и запас на его появление истёк: дальнейшее ожидание
			// заведомо безрезультатно, а до его конца погибший инстанс держит
			// взаимное исключение запусков и расходует время остальных сессий.
			return bootFailed
		}
		if deviceState(ctx, port) == "device" {
			out, err := run(ctx, adb(ctx), "-s", serial(port), "shell", "getprop", "sys.boot_completed")
			if err == nil && strings.TrimSpace(strings.ReplaceAll(out, "\r", "")) == "1" {
				if streak++; streak >= 2 {
					return bootReady
				}
			} else {
				streak = 0
			}
		} else {
			streak = 0
		}
		c.Sleep(3 * time.Second)
	}
}

// bootingErr: отказ, а не молчаливое ожидание. Причина обязана говорить, что
// делать, иначе повторный вызов придёт немедленно и получит то же самое.
func bootingErr(port int) error {
	return fmt.Errorf("instance %s is still booting, retry in a moment", serial(port))
}

func acquire(ctx context.Context, session string, owner int, cwd string) (id string, err error) {
	c := cfg(ctx)
	// Отказы пишутся в журнал одной точкой: путей отказа семь, и каждый из них
	// заканчивается ошибкой. Перевод строки в причине сворачивается — состояние
	// пула в сообщении о исчерпании предела занимает несколько строк.
	defer func() {
		if err != nil {
			logDecision(ctx, "- deny session=%s reason=%s", session, strings.ReplaceAll(err.Error(), "\n", "; "))
		}
	}()
	if session == "" {
		return "", fmt.Errorf("session id required")
	}
	if err := os.MkdirAll(c.Registry, 0o755); err != nil {
		return "", err
	}
	// Крайний срок вычисляется один раз на всю выдачу: складываются именно
	// ожидания внутри неё, поэтому отдельные пределы у каждого из них проблему не
	// решают.
	deadline := c.Now().Add(c.AcquireWait)
	// Отметка до уборки: обращение за инстансом это активность, и своя же уборка
	// не должна завершить инстанс сессии, которая прямо сейчас за ним пришла.
	// Хук отмечает раньше, но подкоманду запускают и руками.
	touch(ctx, session)
	reap(ctx)

	switch port, st := mine(ctx, session, owner, deadline); st {
	case foundReady:
		touchPort(ctx, port)
		return serial(port), nil
	case foundBooting:
		return "", bootingErr(port)
	}

	if !avdExists(ctx) {
		return "", fmt.Errorf("AVD %q not found, prepare it first", c.AVD)
	}

	// Блокировка держится сквозь всю загрузку намеренно.
	lk, err := lock(ctx, deadline)
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
		Image:      c.AVD,
	}); err != nil {
		return "", err
	}

	if err := c.Spawn(logPath(ctx, port), emu(ctx), "@"+c.AVD, "-read-only",
		"-port", strconv.Itoa(port), "-gpu", c.GPU, "-no-window", "-no-audio", "-no-boot-anim"); err != nil {
		dropEntry(ctx, port)
		return "", err
	}
	// Запуск пишется здесь, а не по готовности: место в пределе инстанс занимает
	// уже сейчас, а неудачная загрузка допишет в журнал свой отказ.
	logDecision(ctx, "%s launch session=%s image=%s cwd=%s", serial(port), session, c.AVD, cwd)

	since := c.Now()
	switch waitReady(ctx, port, since, since.Add(c.BootTimeout), deadline) {
	case bootReady:
		touchPort(ctx, port)
		return serial(port), nil
	case bootPending:
		// Загрузку прерывать нельзя: следующее обращение начало бы её заново, и
		// при повторяющихся обращениях пул не пришёл бы в рабочее состояние.
		return "", bootingErr(port)
	}
	killInstance(ctx, port, "reason=boot-timeout session="+session)
	dropEntry(ctx, port)
	return "", fmt.Errorf("instance on port %d did not boot in %s, see %s",
		port, c.BootTimeout, logPath(ctx, port))
}

func avdExists(ctx context.Context) bool {
	c := cfg(ctx)
	out, err := run(ctx, emu(ctx), "-list-avds")
	if err != nil {
		return false
	}
	for line := range strings.SplitSeq(out, "\n") {
		if strings.TrimSpace(line) == c.AVD {
			return true
		}
	}
	return false
}

// release завершает инстанс сессии, в том числе ещё загружающийся: сессии,
// которая его заказала, больше нет, и доводить загрузку до конца незачем.
func release(ctx context.Context, session string, owner int) error {
	port, st := mine(ctx, session, owner, time.Time{})
	if st == foundNone {
		return nil // освобождать нечего это успех
	}
	killInstance(ctx, port, "reason=release session="+session)
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
		// Простой перед рабочим каталогом: каталог может содержать пробелы и
		// поэтому стоит последним.
		fmt.Fprintf(&b, "%s %s %d idle=%s %s\n", serial(port), state, e.OwnerPID, idleText(ctx, e), e.CWD)
	}
	return b.String()
}
