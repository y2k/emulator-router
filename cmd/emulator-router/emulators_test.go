//go:build emulators

// Тесты, реально запускающие эмуляторы. За build-тегом, чтобы обычный прогон
// оставался быстрым и не трогал железо: `make emulators AVD=<образ>`.
//
// Предусловия — в README. Коротко: подготовленный образ со снапшотом загрузки и
// свободные слоты в пределе, потому что тесты занимают их на минуты.
//
// Пропуска по недоступности окружения здесь нет намеренно: пропущенный тест
// читается как пройденный, а эта группа проверяет то, чего не проверяет никто
// другой. Нет образа — тест падает и говорит, чего не хватает.
package main

import (
	"flag"
	"strconv"
	"strings"
	"testing"
)

var liveAVD = flag.String("avd", "", "имя подготовленного образа со снапшотом загрузки")

// liveConfig: настоящий внешний мир и настоящий образ, но свой реестр, свой
// каталог слоёв и свой диапазон портов — чтобы прогон не пересекался с пулом
// работающих сессий.
func liveConfig(t *testing.T) config {
	t.Helper()
	if *liveAVD == "" {
		t.Fatal("не задан образ: go test -tags emulators -run TestLive ./cmd/emulator-router -avd <имя>")
	}
	c := defaultConfig()
	c.AVD = *liveAVD
	c.Registry = t.TempDir()
	c.COWDir = t.TempDir()
	c.PortMin, c.PortMax = 5600, 5610
	c.Max = 2
	if !avdExists(ctxOf(c)) {
		t.Fatalf("образ %q не найден среди доступных: подготовьте его перед прогоном", c.AVD)
	}
	return c
}

// Один прогон, две загрузки: идемпотентность, изоляция, предел и освобождение
// проверяются на одних и тех же инстансах, потому что каждая загрузка стоит минут.
func TestLivePoolLifecycle(t *testing.T) {
	c := liveConfig(t)
	ctx := ctxOf(c)
	t.Cleanup(func() {
		for _, s := range []string{"live-a", "live-b"} {
			release(ctx, s, 0)
		}
	})

	first, err := acquire(ctx, "live-a", 0, t.TempDir())
	if err != nil {
		t.Fatalf("выдача первого инстанса: %v", err)
	}
	if state := deviceState(ctx, portOf(t, first)); state != "device" {
		t.Fatalf("выданный инстанс должен быть готов, состояние %q", state)
	}

	again, err := acquire(ctx, "live-a", 0, t.TempDir())
	if err != nil {
		t.Fatalf("повторная выдача: %v", err)
	}
	if again != first {
		t.Errorf("выдача не идемпотентна: %s затем %s", first, again)
	}

	second, err := acquire(ctx, "live-b", 0, t.TempDir())
	if err != nil {
		t.Fatalf("выдача второго инстанса: %v", err)
	}
	if second == first {
		t.Fatalf("сессии не изолированы: обе получили %s", first)
	}

	// Предел из двух исчерпан: третья сессия должна получить отказ с состоянием
	// пула, а не молча дождаться места.
	if _, err := acquire(ctx, "live-c", 0, t.TempDir()); err == nil {
		t.Error("при исчерпанном пределе ожидался отказ")
	} else if !strings.Contains(err.Error(), first) || !strings.Contains(err.Error(), second) {
		t.Errorf("отказ должен содержать состояние пула, получено: %v", err)
	}

	if err := release(ctx, "live-a", 0); err != nil {
		t.Fatalf("освобождение: %v", err)
	}
	if state := deviceState(ctx, portOf(t, first)); state == "device" {
		t.Errorf("освобождённый инстанс %s всё ещё в перечислении устройств", first)
	}
	if _, ok := readEntry(ctx, portOf(t, first)); ok {
		t.Errorf("запись освобождённого инстанса %s осталась в реестре", first)
	}

	// Освободившееся место возвращается в пул.
	third, err := acquire(ctx, "live-c", 0, t.TempDir())
	if err != nil {
		t.Fatalf("выдача после освобождения: %v", err)
	}
	t.Cleanup(func() { release(ctx, "live-c", 0) })
	if third == second {
		t.Errorf("выдан занятый инстанс %s", third)
	}
}

func portOf(t *testing.T, serial string) int {
	t.Helper()
	port, err := strconv.Atoi(strings.TrimPrefix(serial, "emulator-"))
	if err != nil {
		t.Fatalf("серийник %q без номера порта: %v", serial, err)
	}
	return port
}
