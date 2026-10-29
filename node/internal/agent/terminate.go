package agent

import (
	"bytes"
	"encoding/json"
	"log"
	"math/rand/v2"
	"net"
	"net/http"
	"os"
	"sync"
	"time"

	"sharedd/node/internal/config"
)

const (
	reasonIPBan = "ip_ban"
	reasonDead  = "dead"
	msgIPBan    = "Бан по ip; агент продолжает Globalping и повторит регистрацию после успешной проверки и cooldown"
	msgDead     = "Регистратор не достучался до порта и/или не получил метрики"
)

var agentUnitName = "sharedd-node-agent.service"
var exitProcess = os.Exit // Only the separate network watchdog may request restart.

// Health recovery is state, not a goroutine waiting for an IP change or exit.
// The GP scheduler owns finite recovery attempts; all registration callers share
// one backoff and single-flight gate. No persisted identity/secret is replaced.
type recoveryState struct {
	mu              sync.Mutex
	registering     sync.Mutex
	active          bool
	registered      bool
	bootstrapped    bool // a registration or authenticated config fetch has ever succeeded
	registeredIP    string
	epoch           uint64
	retireIP        string
	next            time.Time
	failures        int
	lastMeasurement string
}

var recovery = &recoveryState{}

func (s *recoveryState) needed() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.active || !s.registered
}

func (s *recoveryState) bootstrapNeeded() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return !s.bootstrapped
}

// One bounded heartbeat bootstrap attempt. Pending credentials can only claim
// via /register, so missing shared config/metrics must not deadlock that claim.
func retryBootstrapRegistration(client *http.Client, cfg *config.NodeConfig, ipr *ipResolver) {
	if cfg == nil || !recovery.bootstrapNeeded() || gate.banActive() {
		return
	}
	if ip, err := ipr.Current(false); err == nil {
		tryRegistration(client, cfg, ip, false)
	}
}

func selfTerminate(_ *config.NodeConfig, reason, message, ip string) {
	recovery.mu.Lock()
	defer recovery.mu.Unlock()
	if recovery.active {
		return
	}
	recovery.active = true
	recovery.registered = false
	recovery.epoch++
	if reason == reasonDead {
		recovery.retireIP = ip
	}
	if message == "" {
		message = msgIPBan
		if reason == reasonDead {
			message = msgDead
		}
	}
	log.Printf("%s; recovery scheduled, Globalping continues (reason=%s)", message, reason)
}

func sendPendingRetire(cfg *config.NodeConfig) {
	recovery.mu.Lock()
	ip := recovery.retireIP
	recovery.retireIP = ""
	recovery.mu.Unlock()
	if cfg == nil || ip == "" {
		return
	}
	body, _ := json.Marshal(map[string]string{"node_id": nodeID, "ip": ip, "reason": reasonDead})
	resp, err := registryRequest(&http.Client{Timeout: 4 * time.Second}, cfg, http.MethodPost, "/retire", bytes.NewReader(body))
	if err != nil {
		log.Printf("retire notice failed: %v", err)
		return
	}
	_ = resp.Body.Close()
}

func tryRegistration(client *http.Client, cfg *config.NodeConfig, ip string, fromGP bool) (bool, time.Duration) {
	if cfg == nil || ip == "" {
		return false, 0
	}
	if !recovery.registering.TryLock() {
		return false, time.Second
	}
	defer recovery.registering.Unlock()
	gate.mu.Lock()
	banUntil := gate.banUntil
	gate.mu.Unlock()
	recovery.mu.Lock()
	if banUntil.After(recovery.next) {
		recovery.next = banUntil
	}
	if wait := time.Until(recovery.next); wait > 0 {
		recovery.mu.Unlock()
		return false, wait
	}
	bootstrap := !recovery.bootstrapped
	if recovery.active && !fromGP && !bootstrap {
		recovery.mu.Unlock()
		return false, 0
	}
	epoch := recovery.epoch
	recovery.mu.Unlock()
	ok, retry := registerWithReason(client, cfg, ip, fromGP, bootstrap)
	recovery.mu.Lock()
	defer recovery.mu.Unlock()
	if ok {
		recovery.bootstrapped = true
		recovery.failures = 0
		recovery.next = time.Time{}
		if epoch == recovery.epoch {
			recovery.active = false
			recovery.registered = true
			recovery.registeredIP = ip
		}
		return true, 0
	}
	recovery.failures++
	if retry <= 0 {
		// Bounded exponential backoff with jitter; no retry loop inside a tick.
		retry = 5 * time.Second * time.Duration(1<<min(recovery.failures-1, 5))
		retry += time.Duration(rand.Int64N(int64(retry / 2)))
		retry = min(retry, 5*time.Minute)
	}
	recovery.next = time.Now().Add(retry)
	return false, retry
}

func isPublicIP(ip string) bool {
	p := net.ParseIP(ip)
	return p != nil && !p.IsPrivate() && !p.IsLoopback() && !p.IsUnspecified()
}
