package main

import (
	"crypto/sha256"
	"crypto/subtle"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/netip"
	"regexp"
	"strings"
	"time"
)

const (
	maxNodeJSONBytes       = 256 << 10
	maxMetricsSnapshotSize = 256
	maxReportAge           = 10 * time.Minute
	maxReportFutureSkew    = time.Minute
)

var (
	nodeIDPattern = regexp.MustCompile(`^[A-Za-z0-9](?:[A-Za-z0-9._-]{0,8}[A-Za-z0-9])?-[a-z0-9]{5}$`)
	nonPublicIPv4 = []netip.Prefix{
		netip.MustParsePrefix("0.0.0.0/8"),
		netip.MustParsePrefix("10.0.0.0/8"),
		netip.MustParsePrefix("100.64.0.0/10"),
		netip.MustParsePrefix("127.0.0.0/8"),
		netip.MustParsePrefix("169.254.0.0/16"),
		netip.MustParsePrefix("172.16.0.0/12"),
		netip.MustParsePrefix("192.0.0.0/24"),
		netip.MustParsePrefix("192.0.2.0/24"),
		netip.MustParsePrefix("192.168.0.0/16"),
		netip.MustParsePrefix("198.18.0.0/15"),
		netip.MustParsePrefix("198.51.100.0/24"),
		netip.MustParsePrefix("203.0.113.0/24"),
		netip.MustParsePrefix("224.0.0.0/4"),
		netip.MustParsePrefix("240.0.0.0/4"),
	}
)

func (r *Registry) nodeAPISecurityEnabled() bool {
	return r != nil && r.cfg != nil && r.cfg.Security.NodeToken != ""
}

// requireNodeToken hashes both values before comparing, keeping the secret
// comparison fixed-width even when a caller supplies a token of another length.
func (r *Registry) requireNodeToken(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		// Production config loading rejects an empty token. This branch permits
		// small unit-test Registry literals that never pass through startup.
		if !r.nodeAPISecurityEnabled() {
			next.ServeHTTP(w, req)
			return
		}
		provided := ""
		if auth := req.Header.Get("Authorization"); strings.HasPrefix(auth, "Bearer ") {
			provided = strings.TrimPrefix(auth, "Bearer ")
		}
		wantHash := sha256.Sum256([]byte(r.cfg.Security.NodeToken))
		gotHash := sha256.Sum256([]byte(provided))
		if subtle.ConstantTimeCompare(wantHash[:], gotHash[:]) != 1 {
			w.Header().Set("WWW-Authenticate", `Bearer realm="node-api"`)
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		next.ServeHTTP(w, req)
	})
}

func decodeNodeJSON(w http.ResponseWriter, req *http.Request, dst any) error {
	req.Body = http.MaxBytesReader(w, req.Body, maxNodeJSONBytes)
	dec := json.NewDecoder(req.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		return err
	}
	if err := dec.Decode(&struct{}{}); err != io.EOF {
		if err == nil {
			return fmt.Errorf("multiple JSON values")
		}
		return err
	}
	return nil
}

func validateNodeID(id string) error {
	if !nodeIDPattern.MatchString(id) {
		return fmt.Errorf("node_id must match NAME-HASH (name 1..10 chars, hash 5 chars)")
	}
	return nil
}

func validatePublicIPv4(s string) error {
	ip, err := netip.ParseAddr(s)
	if err != nil || !ip.Is4() {
		return fmt.Errorf("ip must be an IPv4 address")
	}
	for _, prefix := range nonPublicIPv4 {
		if prefix.Contains(ip) {
			return fmt.Errorf("ip must be public and routable")
		}
	}
	return nil
}

func validateNodeType(nodeType string) error {
	switch nodeType {
	case "classic", "mtproxyl", "meko":
		return nil
	default:
		return fmt.Errorf("node_type must be classic, mtproxyl, or meko")
	}
}

func validateRegisterRequest(body registerRequest) error {
	if err := validateNodeID(body.NodeID); err != nil {
		return err
	}
	if err := validatePublicIPv4(body.IP); err != nil {
		return err
	}
	return validateNodeType(body.NodeType)
}

func validateHealthReport(payload HealthReportPayload, now time.Time) error {
	if err := validateNodeID(payload.NodeID); err != nil {
		return err
	}
	if err := validatePublicIPv4(payload.IP); err != nil {
		return err
	}
	if payload.Port < 1 || payload.Port > 65535 {
		return fmt.Errorf("port must be between 1 and 65535")
	}
	if payload.CheckedAt.IsZero() || now.Sub(payload.CheckedAt) > maxReportAge || payload.CheckedAt.Sub(now) > maxReportFutureSkew {
		return fmt.Errorf("checked_at is outside the accepted freshness window")
	}
	if len(payload.MetricsSnapshot) > maxMetricsSnapshotSize {
		return fmt.Errorf("metrics_snapshot exceeds %d entries", maxMetricsSnapshotSize)
	}
	return nil
}
