package server

// provisionWorker выполняет установку профиля (ожидание RUNNING +
// install-шаги) в фоне, вне 30-секундного запросного context (H1). Платный
// заказ остаётся синхронным (см. handleMeVps): деньги-инвариант «pending ДО
// платного вызова» и has_vps в ответе сохраняются, а vps_ready_timeout_min
// (10м) и install_timeout_min (20м) снова становятся рабочими — их бюджет
// задаёт ProvisionBudget, а не HTTP-таймаут.

import (
	"context"
	"sync"
	"time"
)

// provisionWorker — реестр запущенных операций: одна операция не может
// выполняться дважды (guard по operation id); shutdown отменяет базовый ctx,
// чтобы воркеры не инициировали новые платные вызовы после остановки.
type provisionWorker struct {
	mu      sync.Mutex
	running map[int64]bool
	ctx     context.Context
	cancel  context.CancelFunc
	wg      sync.WaitGroup
}

func newProvisionWorker() *provisionWorker {
	ctx, cancel := context.WithCancel(context.Background())
	return &provisionWorker{running: make(map[int64]bool), ctx: ctx, cancel: cancel}
}

// stop отменяет базовый ctx воркеров и ждёт завершения в полёте, чтобы
// вызывающий не закрыл store под живым provider-вызовом (H5).
func (w *provisionWorker) stop() {
	w.cancel()
	w.wg.Wait()
}

// context — base worker context (cancelled by stop). Fleet workers use it so
// shutdown joins them without borrowing the operation-id namespace.
func (w *provisionWorker) context() context.Context { return w.ctx }

// runningOp — идёт ли уже установка этой операции. Нужно retry-install:
// операцию в installing можно повторить, только если воркер её НЕ выполняет
// (DEF-C: orphaned installing после рестарта не должен блокировать retry).
func (w *provisionWorker) runningOp(opID int64) bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.running[opID]
}

// acquire — занять guard операции для СИНХРОННОГО вызова в текущей
// горутине (blueprint-ран вызывает create в своём воркере и должен сохранить
// recover на том же стеке). false — операция уже выполняется.
func (w *provisionWorker) acquire(opID int64) bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.running[opID] {
		return false
	}
	w.running[opID] = true
	return true
}

// release — освободить guard, занятый acquire.
func (w *provisionWorker) release(opID int64) {
	w.mu.Lock()
	delete(w.running, opID)
	w.mu.Unlock()
}

// run запускает fn в отдельной горутине под таймаутом timeout, привязанным к
// базовому ctx воркера. false — операция уже выполняется (двойной запуск
// невозможен).
func (w *provisionWorker) run(opID int64, timeout time.Duration, fn func(context.Context)) bool {
	w.mu.Lock()
	if w.running[opID] {
		w.mu.Unlock()
		return false
	}
	w.running[opID] = true
	w.wg.Add(1)
	w.mu.Unlock()

	go func() {
		defer w.wg.Done()
		defer func() {
			w.mu.Lock()
			delete(w.running, opID)
			w.mu.Unlock()
		}()
		ctx, cancel := context.WithTimeout(w.ctx, timeout)
		defer cancel()
		fn(ctx)
	}()
	return true
}
