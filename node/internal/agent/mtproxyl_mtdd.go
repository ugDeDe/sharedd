package agent

import (
	"encoding/json"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
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

var readMtproxylMtddUsers = func() ([]mtproxylMtddUser, error) {
	bin, err := mtproxylCLIPath()
	if err != nil {
		return nil, err
	}
	cmd := exec.Command(bin, "secret", "list", "--json")
	cmd.Env = append(os.Environ(), "MTPROXYL_ASSUME_YES=1")
	data, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("read MTProxyL users: %w", err)
	}
	var users []mtproxylMtddUser
	if err := json.Unmarshal(data, &users); err != nil {
		return nil, fmt.Errorf("decode MTProxyL users: %w", err)
	}
	return users, nil
}

// MTProxyL owns secrets.conf and devices.conf; direct API edits would be lost.
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
	names := make([]string, 0, len(shared.Users))
	wanted := make(map[string]string, len(shared.Users))
	for name, secret := range shared.Users {
		if !mtddSecretRe.MatchString(secret) {
			return fmt.Errorf("MTProxyL mtdd: invalid secret for %s", name)
		}
		secret = strings.ToLower(secret)
		if previous, ok := wanted[secret]; ok {
			return fmt.Errorf("MTProxyL mtdd: users %s and %s share a secret", previous, name)
		}
		wanted[secret] = name
		names = append(names, name)
	}
	sort.Strings(names)
	changed := false
	run := func(args ...string) error {
		if err := applyMtproxylCmd(args...); err != nil {
			// The CLI error can contain credentials; do not forward its output.
			return fmt.Errorf("MTProxyL mtdd: %s %s failed", args[0], args[1])
		}
		changed = true
		return nil
	}
	for _, name := range names {
		secret := strings.ToLower(shared.Users[name])
		cur, exists := existing[name]
		if holder := holders[secret]; holder != "" && holder != name {
			if _, sharedHolder := shared.Users[holder]; sharedHolder {
				return fmt.Errorf("MTProxyL mtdd: secret for %s belongs to another shared user", name)
			}
			if !exists {
				if err := run("secret", "rename", holder, name); err != nil {
					return err
				}
				cur, exists = existing[holder], true
				cur.Label = name
			} else if err := run("secret", "remove", holder); err != nil {
				return err
			}
			delete(existing, holder)
			delete(holders, secret)
		}
		if !exists {
			if err := run("secret", "add", name, secret); err != nil {
				return err
			}
			cur = mtproxylMtddUser{Label: name, Secret: secret, Enabled: true}
		} else if !strings.EqualFold(cur.Secret, secret) {
			if err := run("secret", "rotate", name, secret); err != nil {
				return err
			}
			delete(holders, strings.ToLower(cur.Secret))
			cur.Secret = secret
		}
		if !cur.Enabled {
			if err := run("secret", "enable", name); err != nil {
				return err
			}
			cur.Enabled = true
		}
		if shared.TLSDomain != "" {
			domain := strings.ToLower(strings.TrimSpace(shared.TLSDomain))
			if strings.EqualFold(domain, c.TLSDomain) {
				domain = ""
			}
			if !strings.EqualFold(cur.TLSDomain, domain) {
				value := domain
				if value == "" {
					value = "remove"
				}
				if err := run("secret", "domain", name, value); err != nil {
					return err
				}
				cur.TLSDomain = domain
			}
		}
		existing[name], holders[secret] = cur, name
	}
	if changed {
		log.Printf("%s: pool users applied through MTProxyL CLI", mtproxylMtddCapability)
		return applyWaitMetrics(cfg, proxyUpTimeout)
	}
	return nil
}
