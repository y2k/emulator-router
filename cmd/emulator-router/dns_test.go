package main

import (
	"errors"
	"strings"
	"testing"
)

// scutilFixture — вывод `scutil --dns` машины с VPN split DNS, зоны и адреса
// заменены заглушками. Резолвер net.corp.example получил свой сервер, а
// в секцию scoped добавлен домен, чтобы было что различать.
const scutilFixture = `DNS configuration

resolver #1
  search domain[0] : corp.example
  nameserver[0] : 192.0.2.1
  nameserver[1] : 192.0.2.2
  if_index : 14 (en0)
  flags    : Request A records
  reach    : 0x00000002 (Reachable)

resolver #2
  domain   : local
  options  : mdns
  timeout  : 5
  flags    : Request A records
  reach    : 0x00000000 (Not Reachable)
  order    : 300000

resolver #3
  domain   : net.corp.example
  nameserver[0] : 10.1.0.53
  nameserver[1] : 10.1.0.54
  flags    : Supplemental, Request A records
  reach    : 0x00000002 (Reachable)
  order    : 102201

resolver #4
  domain   : corp.example
  nameserver[0] : 10.0.0.53
  nameserver[1] : 10.0.0.54
  flags    : Supplemental, Request A records
  reach    : 0x00000002 (Reachable)
  order    : 102202

resolver #5
  domain   : dup.example
  nameserver[0] : 10.2.0.1
  flags    : Supplemental, Request A records

resolver #6
  domain   : dup.example
  nameserver[0] : 10.2.0.2
  flags    : Supplemental, Request A records

resolver #7
  domain   : 254.169.in-addr.arpa
  options  : mdns
  timeout  : 5
  flags    : Request A records
  reach    : 0x00000000 (Not Reachable)
  order    : 300200

DNS configuration (for scoped queries)

resolver #1
  domain   : scoped.example
  nameserver[0] : 192.0.2.99
  if_index : 14 (en0)
  flags    : Scoped, Request A records
  reach    : 0x00000002 (Reachable)
`

func TestDNSServerFor(t *testing.T) {
	cases := []struct{ name, domain, want string }{
		{"зона резолвера", "corp.example", "10.0.0.53"},
		{"домен внутри зоны резолвера", "s3.stage.corp.example", "10.0.0.53"},
		{"побеждает самая точная зона", "host.net.corp.example", "10.1.0.53"},
		{"граница метки, а не хвост строки", "notcorp.example", ""},
		{"зона резолвера уже домена не подходит", "example", ""},
		{"резолвер без сервера пропускается", "local", ""},
		{"основной резолвер без домена не подходит", "192.0.2.1", ""},
		{"секция scoped не читается", "scoped.example", ""},
		{"посторонняя зона", "other.org", ""},
		{"при равенстве — первый в выводе", "dup.example", "10.2.0.1"},
	}
	for _, tc := range cases {
		if got := serverFor(tc.domain, scutilFixture); got != tc.want {
			t.Errorf("%s: serverFor(%q) = %q, ожидалось %q", tc.name, tc.domain, got, tc.want)
		}
	}
}

func TestWithHostDNS(t *testing.T) {
	c, _ := newTestConfig(t)
	calls := 0
	scutil := func(out string, err error) runner {
		return func(name string, args ...string) (string, error) {
			calls++
			if name != "scutil" {
				t.Fatalf("неожиданный вызов %s", name)
			}
			return out, err
		}
	}

	// Без домена конфигурация машины не читается вовсе: проект без поля не
	// должен платить за него ни поведением, ни вызовом.
	if got := cfg(withHostDNS(ctxOf(c, scutil(scutilFixture, nil)))).DNSServer; got != "" || calls != 0 {
		t.Errorf("без домена: сервер %q, вызовов scutil %d", got, calls)
	}

	c.DNSDomain = "corp.example"
	if got := cfg(withHostDNS(ctxOf(c, scutil(scutilFixture, nil)))).DNSServer; got != "10.0.0.53" {
		t.Errorf("с доменом: сервер %q, ожидался 10.0.0.53", got)
	}
	// Ошибка чтения равна отсутствию резолвера: DNS не предусловие выдачи.
	if got := cfg(withHostDNS(ctxOf(c, scutil(scutilFixture, errors.New("no scutil"))))).DNSServer; got != "" {
		t.Errorf("при ошибке scutil: сервер %q, ожидался пустой", got)
	}
}

// withScutil дополняет fake ответом scutil: остальные вызовы идут в fake как
// есть, и неожиданный вызов по-прежнему ошибка.
func withScutil(f *fake, out *string) runner {
	return func(name string, args ...string) (string, error) {
		if name == "scutil" {
			return *out, nil
		}
		return f.run(name, args...)
	}
}

func TestAcquireLaunchesWithHostDNS(t *testing.T) {
	c, f := newTestConfig(t)
	var args []string
	spawn := c.Spawn
	c.Spawn = func(logPath, name string, a ...string) error {
		args = a
		return spawn(logPath, name, a...)
	}
	out := scutilFixture

	// Без домена флаг не передаётся.
	if _, err := acquire(ctxOf(c, withScutil(f, &out)), "s0", 0, "/w"); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(strings.Join(args, " "), "-dns-server") {
		t.Errorf("без домена передан -dns-server: %v", args)
	}

	c.DNSDomain = "corp.example"
	ctx := ctxOf(c, withScutil(f, &out))
	if _, err := acquire(ctx, "s1", 0, "/w"); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(strings.Join(args, " "), "-dns-server 10.0.0.53") {
		t.Errorf("сервер домена не передан эмулятору: %v", args)
	}
	if e, ok := readEntry(ctx, 15558); !ok || e.DNS != "10.0.0.53" {
		t.Errorf("запись должна помнить DNS-сервер: %+v", e)
	}
	if got := routerLog(t, c); !strings.Contains(got, "launch session=s1 image=pooled_avd dns=10.0.0.53 cwd=/w") {
		t.Errorf("журнал не называет DNS-сервер: %q", got)
	}
}

// Сервер, разошедшийся с текущим (VPN выключили), делает инстанс чужим: иначе
// сессия молча работала бы без DNS.
func TestInstanceWithAnotherDNSServerIsNotHandedBack(t *testing.T) {
	c, f := newTestConfig(t)
	c.DNSDomain = "corp.example"
	out := scutilFixture
	ctx := ctxOf(c, withScutil(f, &out))
	if _, err := acquire(ctx, "s1", 0, "/w"); err != nil {
		t.Fatal(err)
	}

	out = "DNS configuration\n" // резолвера домена больше нет
	if _, err := acquire(ctx, "s1", 0, "/w"); err != nil {
		t.Fatal(err)
	}
	if len(f.spawns) != 2 {
		t.Fatalf("инстанс с новым DNS должен быть запущен: %v", f.spawns)
	}
	if ports := entries(ctx); len(ports) != 1 {
		t.Errorf("прежний инстанс должен быть освобождён, в реестре %v", ports)
	}
	if got := routerLog(t, c); !strings.Contains(got, "kill reason=dns-mismatch was=10.0.0.53 want=none session=s1") {
		t.Errorf("журнал не называет оба сервера: %q", got)
	}
}

// Записи прежних версий DNS-сервера не содержат и совпадают с проектом без домена.
func TestEntryWithoutDNSIsHandedBackWithoutDomain(t *testing.T) {
	c, f := newTestConfig(t)
	ctx := ctxOf(c, f.run)
	if err := writeEntry(ctx, 15556, entry{Session: "s1", Since: f.now, CWD: "/w", Image: c.AVD}); err != nil {
		t.Fatal(err)
	}
	f.devices[15556], f.qemu[15556] = "device", 99999

	serial, err := acquire(ctx, "s1", 0, "/w")
	if err != nil {
		t.Fatal(err)
	}
	if serial != "emulator-15556" || len(f.spawns) != 0 {
		t.Errorf("запись без DNS должна выдаваться как своя: %q, запуски %v", serial, f.spawns)
	}
}

func TestDoctorNamesDNSServerOfDomain(t *testing.T) {
	s := newDoctorStand(t)
	writeSettings(t, s.cwd, `{"avd": "pooled_avd", "dns": "corp.example"}`)
	out := scutilFixture
	s.runner = withScutil(s.f, &out)
	code, report := s.run(t)
	if line := dnsLine(report); code != 0 || !strings.HasPrefix(line, "ok") || !strings.Contains(line, "corp.example resolves through 10.0.0.53") {
		t.Errorf("код=%d, ожидалась успешная проверка с доменом и сервером:\n%s", code, report)
	}
}

// Без VPN пул работает, не резолвится только корпоративная зона: это
// предупреждение, а не нарушение.
func TestDoctorWarnsWithoutResolverForDomain(t *testing.T) {
	s := newDoctorStand(t)
	writeSettings(t, s.cwd, `{"avd": "pooled_avd", "dns": "corp.example"}`)
	out := "DNS configuration\n"
	s.runner = withScutil(s.f, &out)
	code, report := s.run(t)
	if line := dnsLine(report); code != 0 || !strings.HasPrefix(line, "warn") || !strings.Contains(line, "no resolver for corp.example") {
		t.Errorf("код=%d, ожидалось предупреждение, называющее домен:\n%s", code, report)
	}
}

func dnsLine(report string) string {
	for line := range strings.SplitSeq(report, "\n") {
		if strings.Contains(line, "dns:") {
			return line
		}
	}
	return ""
}

func TestDoctorSkipsDNSCheckWithoutDomain(t *testing.T) {
	s := newDoctorStand(t)
	if _, report := s.run(t); dnsLine(report) != "" {
		t.Errorf("без домена проверки DNS быть не должно:\n%s", report)
	}
}
