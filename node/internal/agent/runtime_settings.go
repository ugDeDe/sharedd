package agent

// Применение индивидуальных runtime-настроек ноды (ad_tag).
//
// Поддерживается только MTProxyL: его CLI умеет менять AD_TAG в своём
// источнике настроек, после чего прокси пересобирает config.toml. Classic/MEKO
// не объявляют capability и получают unsupported — мы не пишем неизвестные
// ключи в TOML «на удачу».
//
// Успех фиксируется только после реального перезапуска, проверки /metrics и
// отправки подтверждения на регистратор. Повтор той же ревизии не перезапускает
// прокси.

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"strings"
	"sync"
	"time"

	"sharedd/node/internal/config"
)

type AdTagSetting struct {
	Mode  string `json:"mode"`
	Value string `json:"value,omitempty"`
}

type NodeSettings struct {
	Revision int64        `json:"revision"`
	AdTag    AdTagSetting `json:"ad_tag"`
}

var runtimeSettings struct {
	mu       sync.Mutex
	applied  int64
	reported int64
	prevMode string
	prevTag  string
}

func nodeSupportsAdTagRuntime() bool {
	return mtproxylCLIAvailable() &&
		(applyPreferMtproxyl() || applyDetectNodeType() == NodeTypeMTProxyL)
}

func validAdTag(tag string) bool {
	raw, err := hex.DecodeString(tag)
	return err == nil && len(raw) == 16 && len(tag) == 32
}

// applyNodeSettings — идемпотентное применение desired-ревизии.
// Ошибка не роняет остальные циклы агента: она подтверждается как failed.
func applyNodeSettings(cfg *config.NodeConfig, ns *NodeSettings) {
	if ns == nil || ns.AdTag.Mode == "" || ns.AdTag.Mode == "unmanaged" {
		return
	}
	applySvcMu.Lock()
	defer applySvcMu.Unlock()
	runtimeSettings.mu.Lock()
	applied, reported := runtimeSettings.applied, runtimeSettings.reported
	prevMode, prevTag := runtimeSettings.prevMode, runtimeSettings.prevTag
	runtimeSettings.mu.Unlock()

	if ns.Revision <= reported {
		return
	}
	if ns.Revision <= applied {
		reportNodeSettings(cfg, ns, "applied", applied, "")
		return
	}
	if !nodeSupportsAdTagRuntime() {
		reportNodeSettings(cfg, ns, "unsupported", applied, "agent_capability_missing")
		return
	}
	if ns.AdTag.Mode != "set" {
		// Явное удаление тега через CLI не подтверждено на поддерживаемых
		// версиях; не выдаём ложный успех.
		reportNodeSettings(cfg, ns, "unsupported", applied, "clear_unsupported")
		return
	}
	tag := strings.ToLower(strings.TrimSpace(ns.AdTag.Value))
	if !validAdTag(tag) {
		reportNodeSettings(cfg, ns, "failed", applied, "invalid_ad_tag")
		return
	}

	if err := applyMtproxylCmd("settings", "set", "AD_TAG", tag); err != nil {
		log.Printf("runtime settings: mtproxyl settings set AD_TAG failed: %v", err)
		reportNodeSettings(cfg, ns, "failed", applied, "apply_failed")
		return
	}
	if err := applyMtproxylRestart(); err != nil {
		log.Printf("runtime settings: mtproxyl restart failed: %v", err)
		restoreAdTag(cfg, prevMode, prevTag)
		reportNodeSettings(cfg, ns, "failed", applied, "restart_failed")
		return
	}
	if err := applyWaitMetrics(cfg, proxyUpTimeout); err != nil {
		log.Printf("runtime settings: proxy not ready after ad_tag apply: %v", err)
		restoreAdTag(cfg, prevMode, prevTag)
		reportNodeSettings(cfg, ns, "failed", applied, "not_ready")
		return
	}

	runtimeSettings.mu.Lock()
	runtimeSettings.applied = ns.Revision
	runtimeSettings.prevMode, runtimeSettings.prevTag = ns.AdTag.Mode, tag
	runtimeSettings.mu.Unlock()
	log.Printf("runtime settings: ad_tag applied (revision %d)", ns.Revision)
	reportNodeSettings(cfg, ns, "applied", ns.Revision, "")
}

func restoreAdTag(cfg *config.NodeConfig, mode, tag string) {
	if mode != "set" || !validAdTag(tag) {
		return
	}
	if err := applyMtproxylCmd("settings", "set", "AD_TAG", tag); err != nil {
		log.Printf("runtime settings: restore previous ad_tag failed: %v", err)
		return
	}
	if err := applyMtproxylRestart(); err != nil {
		log.Printf("runtime settings: restart after ad_tag restore failed: %v", err)
		return
	}
	_ = applyWaitMetrics(cfg, proxyRollbackTimeout)
}

func reportNodeSettings(cfg *config.NodeConfig, ns *NodeSettings, state string, applied int64, code string) {
	if ns == nil {
		return
	}
	caps := []string{}
	if nodeSupportsAdTagRuntime() {
		caps = append(caps, "ad_tag")
	}
	body, _ := json.Marshal(map[string]any{
		"node_id":            nodeID,
		"protocol_version":   1,
		"capabilities":       caps,
		"attempted_revision": ns.Revision,
		"applied_revision":   applied,
		"state":              state,
		"error_code":         code,
	})
	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := registryRequest(client, cfg, http.MethodPost, "/api/node/settings/status", bytes.NewReader(body))
	if err != nil {
		log.Printf("runtime settings: report failed: %v (will retry on next sync)", err)
		return
	}
	defer func() { _ = resp.Body.Close() }()
	_, _ = io.Copy(io.Discard, resp.Body)
	if resp.StatusCode != http.StatusOK {
		log.Printf("runtime settings: report rejected: status=%d (will retry on next sync)", resp.StatusCode)
		return
	}
	runtimeSettings.mu.Lock()
	if ns.Revision > runtimeSettings.reported {
		runtimeSettings.reported = ns.Revision
	}
	if state == "applied" && applied > runtimeSettings.applied {
		runtimeSettings.applied = applied
	}
	runtimeSettings.mu.Unlock()
}
