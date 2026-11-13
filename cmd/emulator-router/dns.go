package main

import (
	"context"
	"errors"
	"net"
	"strings"
)

// Корпоративный DNS для инстанса: проект называет домен, а сервер для него
// берётся из резолверов машины. Обоснование и спайк — в
// openspec/changes/archive/2026-11-13-corporate-dns-domain/design.md.

// dnsDomain нормализует домен из настроек. Адрес отвергается: он свойство сети
// машины, и в репозитории оставил бы инстанс без DNS на машине без VPN.
func dnsDomain(raw string) (string, error) {
	d := strings.TrimSuffix(strings.ToLower(strings.TrimSpace(raw)), ".")
	if d == "" || net.ParseIP(d) != nil {
		return "", errors.New(`field "dns" must be a domain name, not a server address: the server is taken from this machine's resolver configuration`)
	}
	return d, nil
}

// withHostDNS — единственный выключатель: без этого вызова в acquire сервер
// всегда пуст, флаг не передаётся, идентичность совпадает. Без домена в
// настройках конфигурация машины не читается.
func withHostDNS(ctx context.Context) context.Context {
	c := cfg(ctx)
	if c.DNSDomain == "" {
		return ctx
	}
	c.DNSServer = ""
	// Ошибка чтения равна отсутствию резолвера: DNS улучшает работу пула, но не
	// предусловие, и блокировать из-за него обращения к устройству хуже.
	if out, err := run(ctx, "scutil", "--dns"); err == nil {
		c.DNSServer = serverFor(c.DNSDomain, out)
	}
	return cfgKey.With(ctx, c)
}

// serverFor выбирает в выводе `scutil --dns` резолвер домена так же, как сама
// macOS: зона равна домену или родительская ему, из подходящих самая точная.
// Секция scoped пропускается: её резолверы привязаны к интерфейсу, а не к зоне.
func serverFor(domain, scutil string) string {
	unscoped, _, _ := strings.Cut(scutil, "DNS configuration (for scoped queries)")
	var zone, server string
	for _, block := range strings.Split(unscoped, "\nresolver #")[1:] {
		var z, s string
		for line := range strings.SplitSeq(block, "\n") {
			k, v, _ := strings.Cut(line, ":")
			switch strings.TrimSpace(k) {
			case "domain":
				z = strings.TrimSuffix(strings.ToLower(strings.TrimSpace(v)), ".")
			case "nameserver[0]":
				s = strings.TrimSpace(v)
			}
		}
		if z != "" && s != "" && len(z) > len(zone) && (domain == z || strings.HasSuffix(domain, "."+z)) {
			zone, server = z, s
		}
	}
	return server
}

func orNone(server string) string {
	if server == "" {
		return "none"
	}
	return server
}

// dnsCheck — проверка для doctor. Отсутствие резолвера — предупреждение: без
// VPN пул работает, не резолвится только корпоративная зона.
func dnsCheck(ctx context.Context) report {
	var r report
	c := cfg(ctx)
	if c.DNSDomain == "" {
		return r
	}
	if s := cfg(withHostDNS(ctx)).DNSServer; s != "" {
		r.add(pass, "dns: %s resolves through %s, instances get it as their DNS server", c.DNSDomain, s)
	} else {
		r.add(warn, "dns: no resolver for %s on this machine (VPN off?): instances launch with the default DNS, and names in %s will not resolve on them", c.DNSDomain, c.DNSDomain)
	}
	return r
}
