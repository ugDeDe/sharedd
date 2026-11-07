package server

// MTProto-проверка прокси ([mtproto_check], internal/mtprotocheck): внешний
// сервис делает настоящий fake-TLS хендшейк общим секретом пула и запрос до
// DC из РФ (за ТСПУ) и снаружи. Идёт параллельно с верификацией Globalping
// в handleHealthReport и смягчает её: Globalping провалена, но MTProto OK —
// нода считается доступной (без карантина и без ухода из очереди).
// Карантин — только когда обе отрицательны. Неоднозначный ответ (UNKNOWN)
// или сбой API — решает одна Globalping, как раньше.

import (
	"context"
	"encoding/hex"
	"fmt"
	"log"
	"sort"
	"strings"
	"time"

	"sharedd/registry/internal/config"
	"sharedd/registry/internal/mtprotocheck"
)

// mtprotoCheckTimeoutSec — таймаут проверки на стороне сервиса (максимум 15).
const mtprotoCheckTimeoutSec = 15

type mtprotoOutcome struct {
	ran        bool
	ok         bool // status OK
	conclusive bool
	status     string // статус сервиса или "error"
	summary    string
	rescued    bool // Globalping провалена, MTProto OK
}

// mtprotoLinkSecret — секрет ссылки пула: ee + секрет первого общего
// пользователя + hex(tls_domain). "" — нет пользователей или домена.
func mtprotoLinkSecret(users map[string]string, tlsDomain string) string {
	if len(users) == 0 || strings.TrimSpace(tlsDomain) == "" {
		return ""
	}
	names := make([]string, 0, len(users))
	for n := range users {
		names = append(names, n)
	}
	sort.Strings(names)
	return "ee" + strings.ToLower(users[names[0]]) + hex.EncodeToString([]byte(tlsDomain))
}

// mtprotoJob — проверка ноды ip:port, если [mtproto_check] включена; nil —
// выключена или секрета нет.
func (r *Registry) mtprotoJob(ip string, port int) func() mtprotoOutcome {
	r.cfgMu.RLock()
	enabled := config.MTProtoCheckEnabled(&r.cfg.RegistryConfig)
	base, token := r.cfg.MTProtoCheck.APIBase, r.cfg.MTProtoCheck.Token
	secret := mtprotoLinkSecret(r.cfg.SharedProxy.Users, r.cfg.SharedProxy.TLSDomain)
	r.cfgMu.RUnlock()
	if !enabled || token == "" || secret == "" || ip == "" || port <= 0 {
		return nil
	}
	check := mtprotoCheckFunc
	return func() mtprotoOutcome {
		ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
		defer cancel()
		res, err := check(ctx, base, token, ip, port, secret)
		if err != nil {
			log.Printf("mtproto check of %s:%d failed: %v", ip, port, err)
			return mtprotoOutcome{ran: true, status: "error", summary: err.Error()}
		}
		ok, conclusive := res.Verdict()
		return mtprotoOutcome{ran: true, ok: ok, conclusive: conclusive, status: res.Status, summary: res.Summary()}
	}
}

// mtprotoCheckFunc — вызов сервиса; var ради тестов.
var mtprotoCheckFunc = func(ctx context.Context, base, token, ip string, port int, secret string) (*mtprotocheck.Result, error) {
	return mtprotocheck.New(base, token).Check(ctx, ip, port, secret, mtprotoCheckTimeoutSec)
}

// noteMTProtoLocked — результат проверки в кандидата и событие, когда
// MTProto впервые удержал ноду с проваленной Globalping от карантина.
func (r *Registry) noteMTProtoLocked(c *Candidate, mt mtprotoOutcome, gpRatio float64) {
	if !mt.ran {
		return
	}
	c.MTProtoStatus, c.MTProtoAt, c.MTProtoSummary = mt.status, time.Now(), mt.summary
	if mt.rescued && !c.MTProtoRescued {
		r.addEventLocked(Event{
			Type: EventMTProtoRescued, NodeID: c.NodeID, IP: c.IP,
			Detail: fmt.Sprintf("Globalping ratio %.2f, но MTProto-проверка: %s — без карантина", gpRatio, mt.summary),
		})
	}
	c.MTProtoRescued = mt.rescued
}
