package main

import (
	"fmt"
	"log"
	"time"
)

func candidateLastGlobalpingAt(c *Candidate) time.Time {
	if !c.LastGlobalpingAt.IsZero() {
		return c.LastGlobalpingAt
	}
	if c.GPLast != nil && !c.GPLast.At.IsZero() {
		return c.GPLast.At
	}
	if n := len(c.GPHist); n > 0 {
		return c.GPHist[n-1].At
	}
	return time.Time{}
}

func globalpingStale(c *Candidate, now time.Time, ttl time.Duration) bool {
	last := candidateLastGlobalpingAt(c)
	return last.IsZero() || now.Sub(last) > ttl
}

// sweepGlobalpingFreshness removes stale GP results from pool eligibility.
// Expiry starts quarantine at zero attempts: only a verified failed
// measurement may increment attempts and eventually cause an IP ban.
func (r *Registry) sweepGlobalpingFreshness(now time.Time) {
	r.cfgMu.RLock()
	ttl := r.cfg.GlobalpingValidityTTL
	r.cfgMu.RUnlock()
	if ttl <= 0 {
		return
	}

	r.mu.Lock()
	defer r.mu.Unlock()
	changed := false
	for _, c := range r.state.Candidates {
		if !globalpingStale(c, now, ttl) || c.Quarantine != nil {
			continue
		}
		last := candidateLastGlobalpingAt(c)
		detail := "globalping result missing"
		if !last.IsZero() {
			detail = fmt.Sprintf("globalping result expired (age %s, ttl %s)", now.Sub(last).Round(time.Second), ttl)
		}
		c.GlobalpingOK = false
		c.Quarantine = &QuarantineState{EnteredAt: now, Attempts: 0, Stale: true}
		r.addEventLocked(Event{Type: EventNodeQuarantined, NodeID: c.NodeID, IP: c.IP, Detail: detail + " — awaiting fresh measurement"})
		log.Printf("candidate %s entered gp quarantine: %s; requesting a fresh check", c.NodeID, detail)
		changed = true
	}
	if changed {
		r.persistStateLocked()
	}
}
