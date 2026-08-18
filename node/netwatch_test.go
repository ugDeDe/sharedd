package main

import (
	"errors"

	"testing"
	"time"
)

// свежий вотчдог для каждого теста + откат подмен
func newTestNetwatch(t *testing.T) *netWatch {
	t.Helper()
	oldDetect, oldRestart := detectOutboundIPv4Fn, restartSelfFn
	t.Cleanup(func() { detectOutboundIPv4Fn, restartSelfFn = oldDetect, oldRestart })
	return &netWatch{}
}

// Смена исходящего IP при недоступном регистраторе → кэш публичного IP
// инвалидируется (следующий Current(false) не отдаст протухший адрес).
func TestNetwatchInvalidatesIPCacheOnOutboundChange(t *testing.T) {
	w := newTestNetwatch(t)
	restartSelfFn = func(string) {}

	ipr := newIPResolver()
	ipr.cached, ipr.fetchedAt, ipr.services = "203.0.113.9", time.Now(), true
	w.bind(nil, ipr)

	detectOutboundIPv4Fn = func() (string, error) { return "10.0.0.5", nil }
	w.noteOK() // эталон: 10.0.0.5

	detectOutboundIPv4Fn = func() (string, error) { return "10.0.0.99", nil }
	w.noteFail()

	ipr.mu.Lock()
	cached := ipr.cached
	ipr.mu.Unlock()
	if cached != "" {
		t.Fatalf("public IP cache must be invalidated after outbound change, still cached %q", cached)
	}
}

// Исходящий IP не менялся → кэш публичного IP НЕ трогаем (обычная временная
// недоступность регистратора не должна дёргать echo-сервисы каждые 15 сек).
func TestNetwatchKeepsIPCacheWhenOutboundSame(t *testing.T) {
	w := newTestNetwatch(t)
	restartSelfFn = func(string) {}

	ipr := newIPResolver()
	ipr.cached, ipr.fetchedAt, ipr.services = "203.0.113.9", time.Now(), true
	w.bind(nil, ipr)

	detectOutboundIPv4Fn = func() (string, error) { return "10.0.0.5", nil }
	w.noteOK()
	w.noteFail()

	ipr.mu.Lock()
	cached := ipr.cached
	ipr.mu.Unlock()
	if cached != "203.0.113.9" {
		t.Fatalf("cache must survive failures without outbound change, got %q", cached)
	}
}

// Рестарт: только при смене исходящего IP И непрерывной серии сбоев дольше
// netRestartAfter. Одиночный сбой рестарта не вызывает.
func TestNetwatchRestartOnlyAfterWindow(t *testing.T) {
	w := newTestNetwatch(t)

	restarts := 0
	restartSelfFn = func(string) { restarts++ }
	detectOutboundIPv4Fn = func() (string, error) { return "10.0.0.5", nil }
	w.noteOK()

	detectOutboundIPv4Fn = func() (string, error) { return "10.0.0.99", nil }
	w.noteFail() // серия только началась — рано
	if restarts != 0 {
		t.Fatal("must not restart on the first failure")
	}

	// серия сбоев «длится» дольше окна
	w.mu.Lock()
	w.firstFailAt = time.Now().Add(-netRestartAfter - time.Second)
	w.mu.Unlock()
	w.noteFail()
	if restarts != 1 {
		t.Fatalf("restart expected after %s of failures with changed IP, got %d", netRestartAfter, restarts)
	}
}

// Без смены исходящего IP рестарта нет, сколько бы ни длились сбои
// (регистратор лежит сам по себе — рестарты ноды бессмысленны и вредны).
func TestNetwatchNoRestartWithoutIPChange(t *testing.T) {
	w := newTestNetwatch(t)

	restarts := 0
	restartSelfFn = func(string) { restarts++ }
	detectOutboundIPv4Fn = func() (string, error) { return "10.0.0.5", nil }
	w.noteOK()

	w.noteFail()
	w.mu.Lock()
	w.firstFailAt = time.Now().Add(-time.Hour)
	w.mu.Unlock()
	w.noteFail()
	if restarts != 0 {
		t.Fatalf("restart without outbound IP change is forbidden, got %d", restarts)
	}
}

// Нет базы для сравнения (агент стартовал в сломанной сети, noteOK ещё не
// было) → ни инвалидации, ни рестартов: после самоперезапуска база пустая,
// значит рестарт-цикла быть не может.
func TestNetwatchNoBaselineNoAction(t *testing.T) {
	w := newTestNetwatch(t)

	restarts := 0
	restartSelfFn = func(string) { restarts++ }
	detectOutboundIPv4Fn = func() (string, error) { return "10.0.0.99", nil }

	ipr := newIPResolver()
	ipr.cached, ipr.fetchedAt, ipr.services = "203.0.113.9", time.Now(), true
	w.bind(nil, ipr)

	w.noteFail()
	w.mu.Lock()
	w.firstFailAt = time.Now().Add(-time.Hour)
	w.mu.Unlock()
	w.noteFail()

	if restarts != 0 {
		t.Fatal("no restart without baseline")
	}
	ipr.mu.Lock()
	cached := ipr.cached
	ipr.mu.Unlock()
	if cached != "203.0.113.9" {
		t.Fatal("cache must not be invalidated without baseline")
	}
}

// noteOK сбрасывает серию: сбой → успех → сбой не суммируются в одно окно.
func TestNetwatchOKResetsFailureWindow(t *testing.T) {
	w := newTestNetwatch(t)

	restarts := 0
	restartSelfFn = func(string) { restarts++ }
	detectOutboundIPv4Fn = func() (string, error) { return "10.0.0.5", nil }
	w.noteOK()

	detectOutboundIPv4Fn = func() (string, error) { return "10.0.0.99", nil }
	w.noteFail()
	w.mu.Lock()
	w.firstFailAt = time.Now().Add(-netRestartAfter + 2*time.Second) // почти дозрели
	w.mu.Unlock()

	detectOutboundIPv4Fn = func() (string, error) { return "10.0.0.99", nil }
	w.noteOK() // регистратор ответил — серия обнуляется (и эталон теперь 10.0.0.99)

	w.noteFail()
	if restarts != 0 {
		t.Fatal("noteOK must reset the failure window")
	}
	w.mu.Lock()
	base := w.goodOutIP
	w.mu.Unlock()
	if base != "10.0.0.99" {
		t.Fatalf("baseline must follow last successful exchange, got %q", base)
	}
}

// detectOutboundIPv4 недоступен (нет маршрута вообще) — вотчдог не паникует
// и не рестартует.
func TestNetwatchDetectErrorTolerated(t *testing.T) {
	w := newTestNetwatch(t)

	restarts := 0
	restartSelfFn = func(string) { restarts++ }
	detectOutboundIPv4Fn = func() (string, error) { return "10.0.0.5", nil }
	w.noteOK()

	detectOutboundIPv4Fn = func() (string, error) { return "", errors.New("network is unreachable") }
	w.noteFail()
	w.mu.Lock()
	w.firstFailAt = time.Now().Add(-time.Hour)
	w.mu.Unlock()
	w.noteFail()
	if restarts != 0 {
		t.Fatal("no restart when outbound detection itself fails")
	}
}

// ipResolver.invalidate: fixed public_ip оператора не трогается.
func TestInvalidateKeepsFixedIP(t *testing.T) {
	r := newIPResolver()
	r.fixed = "198.51.100.1"
	r.invalidate()
	ip, err := r.Current(false)
	if err != nil || ip != "198.51.100.1" {
		t.Fatalf("fixed IP must survive invalidate, got %q err=%v", ip, err)
	}
}

// restartSelf без systemd-юнита НЕ выходит из процесса.
func TestRestartSelfNoSystemdKeepsRunning(t *testing.T) {
	oldExit := exitProcess
	defer func() { exitProcess = oldExit }()
	exited := false
	exitProcess = func(int) { exited = true }

	oldUnit := agentUnitName
	defer func() { agentUnitName = oldUnit }()
	agentUnitName = "definitely-not-a-real-unit-xyz.service"

	restartSelf("test reason")
	if exited {
		t.Fatal("restartSelf must not exit without a loaded systemd unit")
	}
}
