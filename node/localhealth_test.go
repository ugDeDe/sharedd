package main

import (
	"testing"
	"time"
)

// Гейт молчания (V7.9.8; V7.9.11 — GP-нога снята, добавлен dead-килл):
// молчание после 3 подряд локальных падений scrape'а метрик, возврат после
// 2 подряд зелёных; флап серию сбрасывает. Непрерывная немота дольше
// dead_kill — терминальный dead (terminate.go). Немота ПО GLOBALPING на
// ноде больше не существует — судьба GP-больных на регистраторе (карантин).

func TestNetGateMutesAfterThreshold(t *testing.T) {
	g := &netGate{}
	g.noteLocal(false)
	g.noteLocal(false)
	if g.silent() {
		t.Fatal("2 consecutive failures must NOT silence the node yet (threshold 3)")
	}
	g.noteLocal(false)
	if !g.silent() {
		t.Fatal("3 consecutive failures must enter silent healing")
	}
	if !g.metricsMuted() {
		t.Fatal("silence must come from the metrics mute leg")
	}
}

func TestNetGateFlapResetsStreak(t *testing.T) {
	g := &netGate{}
	g.noteLocal(false)
	g.noteLocal(false)
	g.noteLocal(true) // флап: один успех сбрасывает серию падений
	g.noteLocal(false)
	g.noteLocal(false)
	if g.silent() {
		t.Fatal("flapping failures (no 3 in a row) must never silence the node")
	}
}

func TestNetGateRecoveryNeedsTwoGreens(t *testing.T) {
	g := &netGate{}
	for i := 0; i < 3; i++ {
		g.noteLocal(false)
	}
	if !g.silent() {
		t.Fatal("precondition: must be muted")
	}
	g.noteLocal(true)
	if !g.silent() {
		t.Fatal("node must stay silent on a single green check (recover threshold 2)")
	}
	g.noteLocal(false) // флап во время лечения — серия восстановления сброшена
	g.noteLocal(true)
	if !g.silent() {
		t.Fatal("recovery streak must restart after a failure mid-healing")
	}
	g.noteLocal(true)
	if g.silent() {
		t.Fatal("two consecutive green checks must lift the silence")
	}
}

// V7.9.11: часы dead-килла — от входа в немоту, сбрасываются выходом.
func TestNetGateDeadKillClock(t *testing.T) {
	g := &netGate{}
	now := time.Now()
	if g.deadKillDue(now, 10*time.Minute) {
		t.Fatal("healthy gate must never be dead-kill due")
	}
	for i := 0; i < 3; i++ {
		g.noteLocal(false)
	}
	if g.deadKillDue(now, 10*time.Minute) {
		t.Fatal("just-muted gate is not yet due (window 10m)")
	}
	if !g.deadKillDue(now.Add(10*time.Minute+time.Second), 10*time.Minute) {
		t.Fatal("continuous mute beyond the window must be dead-kill due")
	}
	g.noteLocal(true)
	g.noteLocal(true) // вышли из немоты — часы обнулились
	if g.deadKillDue(now.Add(11*time.Minute), 10*time.Minute) {
		t.Fatal("dead-kill clock must reset when silence lifts")
	}
}

// Локальное лечение молчит (немота по метрикам), параллельный активный бан
// её не отменяет; возврат — только позеленение + истёкший бан.
func TestNetGateHealingMuteHoldsWithExpiredBan(t *testing.T) {
	g := &netGate{}
	g.noteBan(time.Now().Add(50 * time.Millisecond))
	for i := 0; i < 3; i++ {
		g.noteLocal(false) // вошли в тихое лечение под активным баном
	}
	time.Sleep(60 * time.Millisecond)
	if !g.silent() {
		t.Fatal("expired ban must NOT lift healing silence (metrics leg holds it)")
	}
	g.noteLocal(true)
	g.noteLocal(true)
	if g.silent() {
		t.Fatal("once healed and ban expired the node must talk again")
	}
}

func TestNetGateBan(t *testing.T) {
	g := &netGate{}
	until := time.Now().Add(time.Hour)
	g.noteBan(until)
	if !g.silent() {
		t.Fatal("ban must silence the node even when locally green")
	}
	if !g.banActive() {
		t.Fatal("ban must be reported active")
	}
	// Поздний короткий бан активный длинный не укорачивает.
	g.noteBan(time.Now().Add(time.Minute))
	g.mu.Lock()
	got := g.banUntil
	g.mu.Unlock()
	if !got.Equal(until) {
		t.Fatalf("shorter later ban must not shorten the active one: got %s want %s", got, until)
	}
	// Dead-килл по бану не срабатывает: часы крутит только метрик-нога.
	if g.deadKillDue(time.Now().Add(24*time.Hour), 10*time.Minute) {
		t.Fatal("prune-ban silence must never trigger the dead-kill clock")
	}
}
