package agent

// Рекламная пауза на MTProxyL: тег пула ставится персональной меткой только
// общим пользователям пула (`mtproxyl secret adtag <пользователь> <тег>`),
// остальные пользователи владельца и его общая метка ([general] ad_tag) не
// трогаются. MTProxyL пишет метку в [access.user_ad_tags] и перезагружает
// конфиг на ходу (SIGHUP) — без перезапуска; telemt берёт метку при новом
// подключении клиента, а клиенты переподключаются за минуты.
//
// Возврат — к той персональной метке, что была у пользователя до рекламы
// (обычно её нет — тогда `remove`, и действует общая метка ноды). Исходные
// метки сохраняются в файл до постановки тега пула: рестарт агента посреди
// рекламного окна не теряет их.
//
// Подтверждение честное: состояние берётся из рабочего config.toml после
// команды, а не из кода возврата CLI. Без ME (use_middle_proxy = false)
// telemt метки не передаёт — такая нода "unsupported".

import (
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/pelletier/go-toml/v2"

	"sharedd/node/internal/config"
)

// adPausePrevFile — метки общих пользователей до рекламы; есть, пока стоит
// тег пула (или его не удалось снять).
var adPausePrevFile = "/var/lib/sharedd/ad_pause_prev.json"

type adPausePrev struct {
	PoolTag string            `json:"pool_tag"`
	Prev    map[string]string `json:"prev"` // пользователь → его метка до рекламы ("" — не было)
}

// adPauseOnMtproxyl — нода под управлением MTProxyL CLI; var ради тестов.
var adPauseOnMtproxyl = func() bool {
	return mtproxylCLIAvailable() && (applyPreferMtproxyl() || applyDetectNodeType() == NodeTypeMTProxyL)
}

// adPauseSharedUsers — общие пользователи пула из последнего /config.
var adPauseSharedUsers = func() []string {
	users := sharedConfigCache.Get().Users
	out := make([]string, 0, len(users))
	for u := range users {
		out = append(out, u)
	}
	sort.Strings(out)
	return out
}

// mtproxylAdView — из рабочего config.toml: включён ли ME и персональные метки.
func mtproxylAdView(path string) (me bool, tags map[string]string, err error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return false, nil, err
	}
	var doc struct {
		General struct {
			UseMiddleProxy *bool `toml:"use_middle_proxy"`
		} `toml:"general"`
		Access struct {
			UserAdTags map[string]string `toml:"user_ad_tags"`
		} `toml:"access"`
	}
	if err := toml.Unmarshal(b, &doc); err != nil {
		return false, nil, fmt.Errorf("parse %s: %w", path, err)
	}
	tags = map[string]string{}
	for u, t := range doc.Access.UserAdTags {
		tags[u] = strings.ToLower(strings.TrimSpace(t))
	}
	// telemt: use_middle_proxy по умолчанию true.
	return doc.General.UseMiddleProxy == nil || *doc.General.UseMiddleProxy, tags, nil
}

func loadAdPausePrev() (*adPausePrev, error) {
	b, err := os.ReadFile(adPausePrevFile)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var p adPausePrev
	if err := json.Unmarshal(b, &p); err != nil {
		return nil, fmt.Errorf("%s: %w", adPausePrevFile, err)
	}
	if p.Prev == nil {
		p.Prev = map[string]string{}
	}
	return &p, nil
}

func saveAdPausePrev(p *adPausePrev) error {
	b, err := json.Marshal(p)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(adPausePrevFile), 0o755); err != nil {
		return err
	}
	tmp := adPausePrevFile + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, adPausePrevFile)
}

// mtproxylSetUserAdTag — персональная метка пользователя; "" снимает её.
func mtproxylSetUserAdTag(user, tag string) error {
	if tag == "" {
		tag = "remove"
	}
	return applyMtproxylCmd("secret", "adtag", user, tag)
}

func applyAdPauseMtproxyl(cfg *config.NodeConfig, d adPauseDirective) string {
	// CLI и пересборку конфига не смешиваем с применением /config.
	applySvcMu.Lock()
	defer applySvcMu.Unlock()

	path := cfg.Telemt.ConfigPath
	me, tags, err := mtproxylAdView(path)
	if err != nil {
		log.Printf("ad pause: %v", err)
		return adPauseFailed
	}
	if !me {
		return adPauseUnsupported // без ME telemt метку не передаёт
	}
	users := adPauseSharedUsers()
	if len(users) == 0 {
		return adPauseFailed // /config ещё не пришёл — общих пользователей не знаем
	}
	prev, err := loadAdPausePrev()
	if err != nil {
		log.Printf("ad pause: %v", err)
		return adPauseFailed
	}

	if d.Active {
		tag := strings.ToLower(strings.TrimSpace(d.Tag))
		if !validAdTag(tag) {
			return adPauseFailed
		}
		if prev == nil {
			prev = &adPausePrev{Prev: map[string]string{}}
		}
		prev.PoolTag = tag
		for _, u := range users {
			if _, ok := prev.Prev[u]; !ok && tags[u] != tag {
				prev.Prev[u] = tags[u]
			}
		}
		// Сначала исходные метки на диск, потом подмена.
		if err := saveAdPausePrev(prev); err != nil {
			log.Printf("ad pause: save %s: %v", adPausePrevFile, err)
			return adPauseFailed
		}
		for _, u := range users {
			if tags[u] == tag {
				continue
			}
			if err := mtproxylSetUserAdTag(u, tag); err != nil {
				log.Printf("ad pause: %v", err)
				return adPauseClassify(err)
			}
		}
		return adPauseConfirm(path, users, tag)
	}

	// Возврат своих меток.
	if prev == nil {
		return adPauseOwn // тег пула не ставили
	}
	for _, u := range users {
		want, known := prev.Prev[u]
		switch {
		case known && tags[u] != want:
		case !known && prev.PoolTag != "" && tags[u] == prev.PoolTag:
			want = "" // пользователь появился посреди окна
		default:
			continue
		}
		if err := mtproxylSetUserAdTag(u, want); err != nil {
			log.Printf("ad pause: restore %s: %v", u, err)
			return adPauseFailed
		}
	}
	if st := adPauseConfirm(path, users, ""); st != adPauseOwn {
		return st
	}
	if err := os.Remove(adPausePrevFile); err != nil && !os.IsNotExist(err) {
		log.Printf("ad pause: remove %s: %v", adPausePrevFile, err)
	}
	return adPauseOwn
}

// adPauseClassify — MTProxyL без `secret adtag` (до 1.4.9) не поддерживает
// персональные метки.
func adPauseClassify(err error) string {
	msg := strings.ToLower(err.Error())
	for _, s := range []string{"неизвестн", "unknown", "usage", "использование"} {
		if strings.Contains(msg, s) {
			return adPauseUnsupported
		}
	}
	return adPauseFailed
}

// adPauseConfirm — что реально в рабочем config.toml: pool — у всех общих
// пользователей тег пула; own — ни у кого из них тега пула нет.
func adPauseConfirm(path string, users []string, poolTag string) string {
	_, tags, err := mtproxylAdView(path)
	if err != nil {
		log.Printf("ad pause: %v", err)
		return adPauseFailed
	}
	if poolTag != "" {
		for _, u := range users {
			if tags[u] != poolTag {
				log.Printf("ad pause: %s has no pool tag in %s after mtproxyl secret adtag", u, path)
				return adPauseFailed
			}
		}
		return adPausePool
	}
	prev, _ := loadAdPausePrev()
	for _, u := range users {
		if prev != nil && prev.PoolTag != "" && tags[u] == prev.PoolTag && prev.Prev[u] != prev.PoolTag {
			return adPauseFailed
		}
	}
	return adPauseOwn
}
