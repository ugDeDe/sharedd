package main

import (
	"context"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/netip"
	"strings"
	"sync"
	"time"
)

// ipEchoServices — сервисы "what is my IP", возвращающие plain-text адрес.
// Запросы форсируются в IPv4 ("tcp4"): DNS-ротация регистратора работает
// с A-записями, поэтому ноде нужен именно публичный IPv4.
// Переменная (не константа) — переопределяется в тестах.
var ipEchoServices = []string{
	"https://api.ipify.org",
	"https://ifconfig.me/ip",
	"https://icanhazip.com",
	"https://checkip.amazonaws.com",
}

// RFC 5737 / RFC 3849 — диапазоны "для документации": типичные значения
// из example-конфигов, которые забывают заменить на реальный IP.
var docIPPrefixes = []netip.Prefix{
	netip.MustParsePrefix("192.0.2.0/24"),
	netip.MustParsePrefix("198.51.100.0/24"),
	netip.MustParsePrefix("203.0.113.0/24"),
	netip.MustParsePrefix("2001:db8::/32"),
}

func newIPv4HTTPClient(timeout time.Duration) *http.Client {
	dialer := &net.Dialer{Timeout: timeout}
	transport := &http.Transport{
		DialContext: func(ctx context.Context, _, addr string) (net.Conn, error) {
			return dialer.DialContext(ctx, "tcp4", addr)
		},
	}
	return &http.Client{Timeout: timeout, Transport: transport}
}

// detectPublicIPFromServices — основной способ: внешние echo-сервисы.
// Принимаем только валидный IPv4 (A-запись), всё остальное — следующий сервис.
func detectPublicIPFromServices() (string, error) {
	client := newIPv4HTTPClient(6 * time.Second)
	var errs []string
	for _, svc := range ipEchoServices {
		resp, err := client.Get(svc)
		if err != nil {
			errs = append(errs, fmt.Sprintf("%s: %v", svc, err))
			continue
		}
		body, err := io.ReadAll(io.LimitReader(resp.Body, 256))
		resp.Body.Close()
		if err != nil || resp.StatusCode != http.StatusOK {
			errs = append(errs, fmt.Sprintf("%s: status=%d err=%v", svc, resp.StatusCode, err))
			continue
		}
		ip, err := netip.ParseAddr(strings.TrimSpace(string(body)))
		if err != nil || !ip.Is4() {
			errs = append(errs, fmt.Sprintf("%s: not an IPv4 literal: %q", svc, strings.TrimSpace(string(body))))
			continue
		}
		return ip.String(), nil
	}
	return "", fmt.Errorf("all IP echo services failed: %s", strings.Join(errs, "; "))
}

// detectOutboundIPv4 — fallback: локальный адрес исходящего маршрута
// (UDP-dial ничего не отправляет, просто спрашивает у ядра исходящий сокет).
// За NAT вернёт приватный адрес — об этом честно предупреждаем.
func detectOutboundIPv4() (string, error) {
	conn, err := net.DialTimeout("udp4", "8.8.8.8:53", 3*time.Second)
	if err != nil {
		return "", fmt.Errorf("outbound interface detection failed: %w", err)
	}
	defer conn.Close()
	addr, ok := conn.LocalAddr().(*net.UDPAddr)
	if !ok || addr.IP == nil {
		return "", fmt.Errorf("unexpected local address %v", conn.LocalAddr())
	}
	return addr.IP.String(), nil
}

const ipCacheTTL = 5 * time.Minute

type ipResolver struct {
	mu        sync.Mutex
	cached    string
	fetchedAt time.Time
	services  bool   // последний успех был через echo-сервисы (не интерфейс)
	fixed     string // [node] public_ip — ручной адрес, детект не нужен
}

func newIPResolver() *ipResolver { return &ipResolver{} }

// invalidate — сброс кэша (сетевой вотчдог заметил смену исходящего IP):
// следующий Current(false) пойдёт в echo-сервисы, а не отдаст протухший
// адрес. Ручной public_ip (fixed) не трогаем — он задан оператором.
func (r *ipResolver) invalidate() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.cached, r.fetchedAt, r.services = "", time.Time{}, false
}

// Current возвращает публичный IPv4 ноды; кэширует на ipCacheTTL.
// force=true игнорирует кэш (старт агента).
func (r *ipResolver) Current(force bool) (string, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	if r.fixed != "" { // ручной публичный адрес
		return r.fixed, nil
	}
	if !force && r.cached != "" && r.services && time.Since(r.fetchedAt) < ipCacheTTL {
		return r.cached, nil
	}

	ip, err := detectPublicIPFromServices()
	if err != nil {
		log.Printf("ip echo services unavailable: %v", err)
		ip, err = detectOutboundIPv4()
		if err != nil {
			return "", err
		}
		log.Printf("WARNING: using outbound interface IP %s (echo сервисов недоступны); за NAT это будет приватный адрес — такая нода бесполезна для пула", ip)
		r.cached, r.fetchedAt, r.services = ip, time.Now(), false
		warnIfNonPublicIP(ip)
		return ip, nil
	}

	if ip != r.cached && r.cached != "" {
		log.Printf("public IP changed: %s -> %s (will re-register)", r.cached, ip)
	}
	r.cached, r.fetchedAt, r.services = ip, time.Now(), true
	return ip, nil
}

func warnIfNonPublicIP(ipStr string) {
	ip, err := netip.ParseAddr(strings.TrimSpace(ipStr))
	if err != nil {
		log.Printf("WARNING: node IP %q is not a valid IP literal", ipStr)
		return
	}
	if ip.IsPrivate() || ip.IsLoopback() || ip.IsLinkLocalUnicast() || ip.IsUnspecified() || !ip.IsGlobalUnicast() {
		log.Printf("WARNING: node IP %q is not public — globalping откажется его проверять (\"private hostname\"), клиенты не смогут подключиться", ipStr)
		return
	}
	for _, p := range docIPPrefixes {
		if p.Contains(ip) {
			log.Printf("WARNING: node IP %q из документационного диапазона %s", ipStr, p)
			return
		}
	}
}
