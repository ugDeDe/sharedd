package main

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func drainGpKick() {
	select {
	case <-gpKick:
	default:
	}
}

// Толчок будит waitGlobalpingTick немедленно, не дожидаясь таймера (5 мин).
func TestKickWakesGlobalpingWait(t *testing.T) {
	drainGpKick()
	done := make(chan struct{})
	go func() {
		waitGlobalpingTick()
		close(done)
	}()
	time.Sleep(50 * time.Millisecond) // цикл повис на ожидании
	kickGlobalping()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("kickGlobalping must wake waitGlobalpingTick immediately")
	}
}

// Без толчка ожидание не завершается раньше таймера.
func TestGlobalpingWaitHoldsWithoutKick(t *testing.T) {
	drainGpKick()
	done := make(chan struct{})
	go func() {
		waitGlobalpingTick()
		close(done)
	}()
	select {
	case <-done:
		t.Fatal("waitGlobalpingTick returned without kick before the timer (default 5m)")
	case <-time.After(300 * time.Millisecond):
	}
	kickGlobalping() // не оставляем горутину висеть
	<-done
}

// Коалесценция: N толчков подряд складываются в ОДИН (буфер 1) —
// серия перерегистраций не жжёт globalping-квоту серией прогонов.
func TestKickCoalesces(t *testing.T) {
	drainGpKick()
	kickGlobalping()
	kickGlobalping()
	kickGlobalping()
	if len(gpKick) != 1 {
		t.Fatalf("kicks must coalesce into one, buffered %d", len(gpKick))
	}
	drainGpKick()
}

// Успешная регистрация (200) ставит толчок; отказ (429) — нет.
func TestRegisterKicksOnlyOn200(t *testing.T) {
	drainGpKick()

	okSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer okSrv.Close()

	nodeID = "n-test-gpkick"
	cfg := &NodeConfig{}
	cfg.Registry.URL = okSrv.URL
	if ok, _ := register(okSrv.Client(), cfg, "203.0.113.9"); !ok {
		t.Fatal("expected successful register")
	}
	if len(gpKick) != 1 {
		t.Fatal("successful register must queue a globalping kick")
	}
	drainGpKick()

	quarSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Retry-After", "900")
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	defer quarSrv.Close()
	cfg2 := &NodeConfig{}
	cfg2.Registry.URL = quarSrv.URL
	if ok, _ := register(quarSrv.Client(), cfg2, "203.0.113.9"); ok {
		t.Fatal("429 register must not be ok")
	}
	if len(gpKick) != 0 {
		t.Fatal("rejected register must NOT queue a globalping kick")
	}
}
