package agent

import (
	"bufio"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"sharedd/node/internal/config"
)

// Marker lets installers reject an older agent before replacing the binary.
const mtproxylMtddCapability = "MTPROXYL-MTDD"

func isMtproxylMtddConfig(path string) bool {
	return filepath.Clean(path) == filepath.Join(mtproxylInstallDir, "mtdd", "config.json")
}

type mtproxylMtddUser struct {
	Label     string `json:"label"`
	Secret    string `json:"secret"`
	Enabled   bool   `json:"enabled"`
	TLSDomain string `json:"tls_domain"`
	AdTag     string `json:"ad_tag"`
}

// mtproxylLabelRe — правило меток MTProxyL (secret add/rename): имена с точкой,
// которые допускает регистратор, CLI менеджера отвергает.
var mtproxylLabelRe = regexp.MustCompile(`^[a-zA-Z0-9_-]{1,32}$`)

var mtproxylHexRe = regexp.MustCompile(`[0-9a-fA-F]{16,}`)

// mtproxylCmdError — ошибка CLI без секретов и токенов: длинные hex-строки
// вырезаются, причина остаётся видна в журнале.
func mtproxylCmdError(args []string, err error) error {
	what := strings.Join(args[:min(2, len(args))], " ")
	return fmt.Errorf("MTProxyL mtdd: %s: %s", what, mtproxylHexRe.ReplaceAllString(err.Error(), "<redacted>"))
}

// readMtproxylMtddUsers — пользователи менеджера прямо из его баз, только
// чтение: secrets.conf (LABEL|SECRET|CREATED|ENABLED|…|AD_TAG|RATE) и
// devices.conf (LABEL|DEVICES|TLS_DOMAIN|CANARY). `mtproxyl secret list --json`
// на каждом цикле синхронизации ходил бы в API движка, дописывал историю IP и
// считал статистику всех пользователей. Правила разбора — как load_secrets.
var readMtproxylMtddUsers = func() ([]mtproxylMtddUser, error) {
	domains := map[string]string{}
	if err := scanPipeFile(filepath.Join(mtproxylInstallDir, "devices.conf"), func(f []string) {
		if len(f) > 2 && f[2] != "" {
			domains[f[0]] = strings.ToLower(strings.TrimSpace(f[2]))
		}
	}); err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("read MTProxyL devices: %w", err)
	}
	var users []mtproxylMtddUser
	err := scanPipeFile(filepath.Join(mtproxylInstallDir, "secrets.conf"), func(f []string) {
		if len(f) < 2 || !mtproxylLabelRe.MatchString(f[0]) || !mtddSecretRe.MatchString(f[1]) {
			return
		}
		u := mtproxylMtddUser{Label: f[0], Secret: strings.ToLower(f[1]), Enabled: true, TLSDomain: domains[f[0]]}
		if len(f) > 3 && f[3] == "false" {
			u.Enabled = false
		}
		if len(f) > 9 && validAdTag(strings.ToLower(f[9])) {
			u.AdTag = strings.ToLower(f[9])
		}
		users = append(users, u)
	})
	if err != nil {
		return nil, fmt.Errorf("read MTProxyL secrets: %w", err)
	}
	return users, nil
}

// scanPipeFile — строки «a|b|…» без комментариев и пустых.
func scanPipeFile(path string, row func([]string)) error {
	fh, err := os.Open(path)
	if err != nil {
		return err
	}
	defer func() { _ = fh.Close() }()
	sc := bufio.NewScanner(fh)
	sc.Buffer(make([]byte, 64*1024), 1024*1024)
	for sc.Scan() {
		line := strings.TrimRight(sc.Text(), "\r")
		if t := strings.TrimSpace(line); t == "" || strings.HasPrefix(t, "#") {
			continue
		}
		row(strings.Split(line, "|"))
	}
	return sc.Err()
}

// applyMtproxylMtddShared — пользователи пула на MTProxyL Manager с движком
// mtdd. MTProxyL владеет secrets.conf и devices.conf и пересобирает из них
// пользователей mtdd: правки через API mtdd потерялись бы, поэтому изменения
// идут только через `mtproxyl secret …` (применяются на лету, без рестарта).
// Ошибка одного пользователя не останавливает остальных — как у чистого mtdd.
func applyMtproxylMtddShared(cfg *config.NodeConfig, shared SharedConfig) error {
	c, err := loadMtddConfig(cfg.Telemt.ConfigPath)
	if err != nil {
		return err
	}
	users, err := readMtproxylMtddUsers()
	if err != nil {
		return err
	}
	existing := make(map[string]mtproxylMtddUser, len(users))
	holders := make(map[string]string, len(users))
	for _, u := range users {
		existing[u.Label] = u
		holders[strings.ToLower(u.Secret)] = u.Label
	}

	var firstErr error
	note := func(e error) {
		log.Printf("%v", e)
		if firstErr == nil {
			firstErr = e
		}
	}
	names := make([]string, 0, len(shared.Users))
	wanted := make(map[string]string, len(shared.Users))
	for name, secret := range shared.Users {
		names = append(names, name)
		wanted[name] = strings.ToLower(secret)
	}
	sort.Strings(names)
	owner := make(map[string]string, len(names))
	valid := make([]string, 0, len(names))
	for _, name := range names {
		secret := wanted[name]
		switch {
		case !mtproxylLabelRe.MatchString(name):
			note(fmt.Errorf("MTProxyL mtdd: user %q skipped: MTProxyL labels allow only a-z, A-Z, 0-9, _ and - (up to 32)", name))
		case !mtddSecretRe.MatchString(secret):
			note(fmt.Errorf("MTProxyL mtdd: user %q skipped: invalid secret", name))
		case owner[secret] != "":
			note(fmt.Errorf("MTProxyL mtdd: users %s and %s share a secret", owner[secret], name))
		default:
			owner[secret] = name
			valid = append(valid, name)
		}
	}

	changed := false
	run := func(args ...string) error {
		if err := applyMtproxylCmd(args...); err != nil {
			return mtproxylCmdError(args, err)
		}
		changed = true
		return nil
	}
	domain := strings.ToLower(strings.TrimSpace(shared.TLSDomain))
	if strings.EqualFold(domain, c.TLSDomain) {
		domain = "" // общий домен сервера — персональный не нужен
	}
	for _, name := range valid {
		secret := wanted[name]
		cur, exists := existing[name]
		if holder := holders[secret]; holder != "" && holder != name {
			if _, sharedHolder := shared.Users[holder]; sharedHolder {
				note(fmt.Errorf("MTProxyL mtdd: secret for %s belongs to another shared user", name))
				continue
			}
			// Секрет у локального пользователя: переименование общего
			// пользователя в регистраторе или устаревший псевдоним.
			if !exists {
				if err := run("secret", "rename", holder, name); err != nil {
					note(err)
					continue
				}
				cur, exists = existing[holder], true
				cur.Label = name
			} else if err := run("secret", "remove", holder); err != nil {
				note(err)
				continue
			}
			delete(existing, holder)
			delete(holders, secret)
		}
		if !exists {
			if err := run("secret", "add", name, secret); err != nil {
				note(err)
				continue
			}
			cur = mtproxylMtddUser{Label: name, Secret: secret, Enabled: true}
		} else if !strings.EqualFold(cur.Secret, secret) {
			if err := run("secret", "rotate", name, secret); err != nil {
				note(err)
				continue
			}
			delete(holders, strings.ToLower(cur.Secret))
			cur.Secret = secret
		}
		holders[secret] = name
		if !cur.Enabled {
			if err := run("secret", "enable", name); err != nil {
				note(err)
			} else {
				cur.Enabled = true
			}
		}
		if shared.TLSDomain != "" && !strings.EqualFold(cur.TLSDomain, domain) {
			value := domain
			if value == "" {
				value = "remove"
			}
			if err := run("secret", "domain", name, value); err != nil {
				note(err)
			} else {
				cur.TLSDomain = domain
			}
		}
		existing[name] = cur
	}
	if changed {
		log.Printf("%s: pool users applied through MTProxyL CLI", mtproxylMtddCapability)
		if err := applyWaitMetrics(cfg, proxyUpTimeout); err != nil {
			return err
		}
	}
	return firstErr
}
