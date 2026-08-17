package main

import (
	"net"
	"os"
	"strconv"
	"testing"
	"time"
)

// Globalping-цикл ждёт listen-порт прокси, прежде чем создавать
// measurement — иначе пробы летят в закрытый порт и верификация рисует 0.
func TestWaitProxyTCP(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer ln.Close()
	port := ln.Addr().(*net.TCPAddr).Port

	if !waitProxyTCP(port, 2*time.Second) {
		t.Fatal("listening port must be detected as ready")
	}

	// закрытый порт + короткий таймаут → false (не висим вечно)
	ln2, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen2: %v", err)
	}
	closedPort := ln2.Addr().(*net.TCPAddr).Port
	ln2.Close()

	start := time.Now()
	if waitProxyTCP(closedPort, 500*time.Millisecond) {
		t.Fatal("closed port must not be ready")
	}
	if time.Since(start) > 5*time.Second {
		t.Fatalf("wait did not respect timeout: %v", time.Since(start))
	}

	// порт появляется ПОЗДНЕЕ — дожидаемся
	ln3, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen3: %v", err)
	}
	latePort := ln3.Addr().(*net.TCPAddr).Port
	ln3.Close()

	go func() {
		time.Sleep(700 * time.Millisecond)
		ln4, err := net.Listen("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(latePort)))
		if err == nil {
			time.Sleep(2 * time.Second) // подержать listen до конца проверки
			ln4.Close()
		}
	}()
	if !waitProxyTCP(latePort, 4*time.Second) {
		t.Fatal("must wait for the port that appears later")
	}
}

// Идемпотентность патча: повторное применение того же /config не меняет файл
// (changed=false) → боевой конвейер НЕ рестартует прокси без нужды.
func TestApplySharedConfigAdditivelyReportsChange(t *testing.T) {
	dir := t.TempDir()
	cfgPath := dir + "/telemt.toml"
	base := "[general]\n\n[censorship]\ntls_domain = \"old.example.com\"\n"
	if err := os.WriteFile(cfgPath, []byte(base), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg := &NodeConfig{}
	cfg.Telemt.ConfigPath = cfgPath

	shared := SharedConfig{
		TLSDomain: "m.beboo.ru",
		Users:     map[string]string{"alice": "0123456789abcdef0123456789abcdef"},
	}
	changed, err := applySharedConfigAdditively(cfg, shared)
	if err != nil {
		t.Fatalf("first apply: %v", err)
	}
	if !changed {
		t.Fatal("first apply must report changed=true")
	}
	changed, err = applySharedConfigAdditively(cfg, shared)
	if err != nil {
		t.Fatalf("second apply: %v", err)
	}
	if changed {
		t.Fatal("second apply of identical config must report changed=false (no restart needed)")
	}
}
