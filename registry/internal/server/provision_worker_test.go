package server

// H1: фоновый воркер установки — один запуск на операцию (guard), отвязанный
// от запросного ctx бюджет, чистая остановка.

import (
	"context"
	"sync/atomic"
	"testing"
	"time"
)

// Воркер не выполняет одну операцию дважды и снимает guard после завершения.
func TestProvisionWorkerGuardsOperation(t *testing.T) {
	w := newProvisionWorker()
	defer w.stop()

	var done atomic.Bool
	started := make(chan struct{})
	release := make(chan struct{})

	if !w.run(7, time.Minute, func(context.Context) {
		close(started)
		<-release
		done.Store(true)
	}) {
		t.Fatal("first run must start")
	}
	<-started
	if w.run(7, time.Minute, func(context.Context) { t.Error("double-run must never happen") }) {
		t.Fatal("second run of the same op must be refused")
	}
	close(release)

	// дождаться завершения: guard освобождается, повторный запуск снова возможен.
	deadline := time.Now().Add(2 * time.Second)
	for {
		if !done.Load() {
			if time.Now().After(deadline) {
				t.Fatal("worker fn did not complete")
			}
			time.Sleep(time.Millisecond)
			continue
		}
		if w.run(7, time.Minute, func(context.Context) {}) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("guard was not released after completion")
		}
		time.Sleep(time.Millisecond)
	}
}

// Воркер использует собственный (отвязанный от запроса) context: stop()
// отменяет его, а до stop он жив, хотя запросного ctx уже нет.
func TestProvisionWorkerDetachedCtxAndStop(t *testing.T) {
	w := newProvisionWorker()
	got := make(chan context.Context, 1)
	if !w.run(1, time.Hour, func(ctx context.Context) {
		got <- ctx
		<-ctx.Done() // держим воркер живым до stop()
	}) {
		t.Fatal("run must start")
	}
	ctx := <-got
	if ctx.Err() != nil {
		t.Fatalf("worker ctx must be alive (detached), got %v", ctx.Err())
	}
	w.stop()
	select {
	case <-ctx.Done():
	case <-time.After(2 * time.Second):
		t.Fatal("stop() must cancel the worker base ctx")
	}
}
