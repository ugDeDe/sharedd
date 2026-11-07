package agent

// Рекламная пауза пула: регистратор в ответе на heartbeat говорит, какой тег
// держать сейчас (тег пула или свой), агент применяет и подтверждает
// фактическое состояние в следующем heartbeat. Решает только регистратор.
//
// Поддерживаются:
//   - mtdd с подменой тега на лету (POST/DELETE /v1/ad-tag): тег меняется без
//     перезапуска и без переподключения клиентов, а «свой» тег — это ad_tag
//     из /etc/mtdd/config.json, к которому возвращает DELETE (подмена не
//     пишется в файл, после рестарта mtdd снова свой тег);
//   - MTProxyL: персональная метка только общих пользователей пула
//     (ad_pause_mtproxyl.go), горячая перезагрузка без перезапуска.
// Прочие ноды подтверждают "unsupported" и в расписание не попадают.

import (
	"encoding/json"
	"log"
	"net/http"
	"strings"
	"sync"
	"time"

	"sharedd/node/internal/config"
)

const (
	adPausePool        = "pool"
	adPauseOwn         = "own"
	adPauseUnsupported = "unsupported"
	adPauseFailed      = "failed"
	// Повтор после неудачи и перепроверка поддержки (mtdd могли обновить).
	adPauseRetry       = 30 * time.Second
	adPauseRecheckUnsp = 10 * time.Minute
)

type adPauseDirective struct {
	Active bool   `json:"active"`
	Tag    string `json:"tag,omitempty"`
}

type heartbeatResponse struct {
	AdPause *adPauseDirective `json:"ad_pause,omitempty"`
}

var adPause = struct {
	mu sync.Mutex
	// supported — регистратор прислал ad_pause: он понимает ad_pause_state.
	supported bool
	// state — подтверждаемое регистратору фактическое состояние тега.
	state string
	// attempted/failed/at — последняя попытка применить указание;
	// users — общие пользователи на момент попытки (MTProxyL ставит метку
	// каждому, новый пользователь посреди окна требует повтора).
	attempted adPauseDirective
	users     string
	failed    bool
	at        time.Time
	desired   chan adPauseDirective
}{desired: make(chan adPauseDirective, 1)}

// adPauseHeartbeatState — что подтвердить в heartbeat ("" — поле не слать:
// прежний регистратор отверг бы неизвестное поле).
func adPauseHeartbeatState() string {
	adPause.mu.Lock()
	defer adPause.mu.Unlock()
	if !adPause.supported {
		return ""
	}
	return adPause.state
}

// noteHeartbeatResponse — тело 200 на heartbeat. Прежний регистратор отвечает
// пустым телом: тогда держим свой тег.
func noteHeartbeatResponse(body []byte) {
	var resp heartbeatResponse
	_ = json.Unmarshal(body, &resp)
	adPause.mu.Lock()
	adPause.supported = resp.AdPause != nil
	adPause.mu.Unlock()
	d := adPauseDirective{}
	if resp.AdPause != nil {
		d = *resp.AdPause
	}
	select {
	case <-adPause.desired: // заменить непрочитанное устаревшее указание
	default:
	}
	adPause.desired <- d
}

func adPauseLoop(cfg *config.NodeConfig) {
	for d := range adPause.desired {
		adPauseStep(cfg, d, time.Now())
	}
}

// adPauseStep — одно указание регистратора: применить при необходимости и
// запомнить фактическое состояние для следующего heartbeat.
func adPauseStep(cfg *config.NodeConfig, d adPauseDirective, now time.Time) {
	users := strings.Join(adPauseSharedUsers(), ",")
	adPause.mu.Lock()
	state, attempted, failed, at, prevUsers := adPause.state, adPause.attempted, adPause.failed, adPause.at, adPause.users
	adPause.mu.Unlock()
	if !adPauseNeedsApply(d, state, attempted, failed, at, now) && (users == prevUsers || state != adPausePool) {
		return
	}
	next := applyAdPause(cfg, d)
	ok := next != adPauseFailed
	if !ok && !d.Active && state == adPausePool {
		// Вернуть свой тег не вышло — на ноде по-прежнему тег пула, и
		// регистратор должен продолжать учитывать это время.
		next = adPausePool
	}
	adPause.mu.Lock()
	adPause.state, adPause.attempted, adPause.failed, adPause.at, adPause.users = next, d, !ok, now, users
	adPause.mu.Unlock()
	if next != state {
		log.Printf("ad pause: active=%t → %s", d.Active, next)
	}
}

// adPauseNeedsApply — API mtdd трогаем только при смене указания, для повтора
// после ошибки и для редкой перепроверки поддержки.
func adPauseNeedsApply(d adPauseDirective, state string, attempted adPauseDirective, failed bool, at, now time.Time) bool {
	switch {
	case state == "":
		return true // первый запуск: снять возможную подмену упавшего агента
	case d != attempted:
		return true
	case failed:
		return now.Sub(at) >= adPauseRetry
	case state == adPauseUnsupported:
		return now.Sub(at) >= adPauseRecheckUnsp
	default:
		return false
	}
}

var applyAdPause = func(cfg *config.NodeConfig, d adPauseDirective) string {
	switch {
	case cfg == nil:
		return adPauseUnsupported
	case isMtddConfigPath(cfg.Telemt.ConfigPath):
		return applyAdPauseMtdd(cfg, d)
	case adPauseOnMtproxyl():
		return applyAdPauseMtproxyl(cfg, d)
	default:
		return adPauseUnsupported
	}
}

func applyAdPauseMtdd(cfg *config.NodeConfig, d adPauseDirective) string {
	mc, err := loadMtddConfig(cfg.Telemt.ConfigPath)
	if err != nil {
		log.Printf("ad pause: %v", err)
		return adPauseFailed
	}
	if d.Active {
		tag := strings.ToLower(strings.TrimSpace(d.Tag))
		if !validAdTag(tag) {
			return adPauseFailed
		}
		err = mtddAPI(mc, http.MethodPost, "/v1/ad-tag", map[string]string{"ad_tag": tag})
	} else {
		err = mtddAPI(mc, http.MethodDelete, "/v1/ad-tag", nil)
	}
	switch {
	case err == nil && d.Active:
		return adPausePool
	case err == nil:
		return adPauseOwn
	case strings.Contains(err.Error(), "HTTP 404"):
		return adPauseUnsupported // mtdd без /v1/ad-tag: тег всегда свой
	default:
		log.Printf("ad pause: %v", err)
		return adPauseFailed
	}
}
