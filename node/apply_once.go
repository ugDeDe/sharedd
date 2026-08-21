package main

// Однократное применение конфига — режим `-apply-once` для
// установщика. Раньше установщик сам останавливал прокси, а дальше «надеялся»,
// что свежезапущенный агент успешно сходит на регистратор и поднимет прокси
// обратно: любой сбой /config (DNS, TLS, таймаут) оставлял прокси лежать,
// первый globalping рисовал 0.
//
// Теперь весь конвейер выполняется СИНХРОННО одним прогоном агента, ДО старта
// демона sharedd-node-agent.service:
//
//	fetch /config (с ретраями) → applySharedConfigManaged
//	(стоп прокси → патч telemt.toml → старт → ожидание /metrics → откат)
//	→ ensureProxyUp (финальное «запустить прокси»: метрики обязаны отвечать,
//	иначе (пере)запускаем — юнит / mtproxyl CLI / вслепую telemt.service)
//
// Коды выхода — контракт с установщиком:
//
//	0 — прокси поднят, конфиг применён (или уже был актуален), метрики отвечают;
//	1 — применить/поднять не удалось (установщик делает rescue-рестарт);
//	3 — регистратор недоступен: НИЧЕГО не трогали, прокси в прежнем состоянии
//	 (агент-демон догонит конфиг фоновым sync, когда регистратор вернётся).
//
// Маркерные строки `APPLY-ONCE: ...` печатаются в stdout — установщик грепает
// их (а `grep -q 'APPLY-ONCE:'` по самому бинарнику отличает сборку с этим
// режимом от старой, у которой неизвестный флаг просто игнорировался бы).
//
// Без регистратора (код 3) принципиально НЕ патчим: пустой shared-конфиг
// записал бы «половину» состояния и подписал бы ноду под лишний рестарт,
// когда регистратор вернётся.

import (
	"fmt"
	"log"
	"time"
)

const (
	applyOnceOK         = 0
	applyOnceFailed     = 1
	applyOnceNoRegistry = 3
)

// Ретраи /config — var ради тестов. На свежей ноде сеть/DNS могут ещё
// раскачиваться, поэтому даём регистратору несколько шансов.
var (
	applyOnceFetchAttempts = 6
	applyOnceFetchDelay    = 3 * time.Second
)

func runApplyOnce(cfg *NodeConfig) int {
	if !cfg.Sync.ApplyToTelemt {
		fmt.Println("APPLY-ONCE: skipped (sync.apply_to_telemt = false)")
		return applyOnceOK
	}

	// 1. общий конфиг регистратора
	var shared SharedConfig
	var ferr error
	for attempt := 1; attempt <= applyOnceFetchAttempts; attempt++ {
		shared, ferr = fetchSharedConfig(cfg)
		if ferr == nil {
			break
		}
		log.Printf("apply-once: fetch /config attempt %d/%d failed: %v", attempt, applyOnceFetchAttempts, ferr)
		if attempt < applyOnceFetchAttempts {
			time.Sleep(applyOnceFetchDelay)
		}
	}
	if ferr != nil {
		fmt.Printf("APPLY-ONCE: registry-unreachable: %v\n", ferr)
		return applyOnceNoRegistry
	}

	// 2. конвейер: стоп → патч+сохранение → старт → ожидание /metrics → откат
	applyErr := applySharedConfigManaged(cfg, shared)

	// 3. финальный «запустить прокси»: метрики обязаны отвечать — даже если
	// правок не было (changed=false), прокси мог лежать ещё до установки.
	upErr := ensureProxyUp(cfg)

	switch {
	case applyErr == nil && upErr == nil:
		fmt.Println("APPLY-ONCE: ok")
		return applyOnceOK
	case applyErr != nil:
		fmt.Printf("APPLY-ONCE: failed: %v\n", applyErr)
		if upErr != nil {
			fmt.Printf("APPLY-ONCE: proxy still down: %v\n", upErr)
		}
		return applyOnceFailed
	default:
		fmt.Printf("APPLY-ONCE: failed: proxy not up: %v\n", upErr)
		return applyOnceFailed
	}
}
