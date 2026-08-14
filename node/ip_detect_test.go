package main

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestDetectPublicIPFromServices(t *testing.T) {
	garbage := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, "<html>not an ip</html>")
	}))
	defer garbage.Close()

	// имитация ifconfig на v6-only машине — должен быть пропущен (нужен IPv4)
	v6 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, "2001:db8::1")
	}))
	defer v6.Close()

	good := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, "198.51.100.77\n")
	}))
	defer good.Close()

	old := ipEchoServices
	ipEchoServices = []string{garbage.URL, v6.URL, good.URL}
	defer func() { ipEchoServices = old }()

	ip, err := detectPublicIPFromServices()
	if err != nil {
		t.Fatal(err)
	}
	if ip != "198.51.100.77" {
		t.Fatalf("expected first valid IPv4, got %q", ip)
	}
}

func TestDetectPublicIPAllDown(t *testing.T) {
	old := ipEchoServices
	ipEchoServices = []string{"http://127.0.0.1:1", "http://127.0.0.1:2"}
	defer func() { ipEchoServices = old }()

	if _, err := detectPublicIPFromServices(); err == nil {
		t.Fatal("must fail when all services are down")
	}
}

func TestIPResolverCachesAndPrefersServices(t *testing.T) {
	// v6 захардкожен в ядре маршрутизации... упрощённо: проверяем, что при
	// живом сервисе resolver берёт IP с него, а не с интерфейса.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, "203.0.113.9")
	}))
	defer srv.Close()

	old := ipEchoServices
	ipEchoServices = []string{srv.URL}
	defer func() { ipEchoServices = old }()

	r := newIPResolver()
	ip, err := r.Current(true)
	if err != nil {
		t.Fatal(err)
	}
	if ip != "203.0.113.9" {
		t.Fatalf("got %q", ip)
	}

	// кэш: сервис «сломался», но Current(false) в пределах TTL отдаёт кэш
	ipEchoServices = []string{"http://127.0.0.1:1"}
	ip2, err := r.Current(false)
	if err != nil {
		t.Fatal(err)
	}
	if ip2 != ip {
		t.Fatalf("cached value expected, got %q", ip2)
	}
	if !strings.HasPrefix(ip2, "203.0.113.") {
		t.Fatal("sanity")
	}
}
