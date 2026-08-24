package machineapi

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/netip"
	"regexp"
	"time"
)

const (
	MaxNodeJSONBytes       = 256 << 10
	MaxMetricsSnapshotSize = 256
	MaxReportAge           = 10 * time.Minute
	MaxReportFutureSkew    = time.Minute
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

// DecodeJSON — строгое чтение JSON-тела: лимит размера, запрет неизвестных
// полей, ровно одно значение.
func DecodeJSON(w http.ResponseWriter, req *http.Request, dst any) error {
	req.Body = http.MaxBytesReader(w, req.Body, MaxNodeJSONBytes)
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

func ValidateNodeID(id string) error {
	if !nodeIDPattern.MatchString(id) {
		return fmt.Errorf("node_id must match NAME-HASH (name 1..10 chars, hash 5 chars)")
	}
	return nil
}

func ValidatePublicIPv4(s string) error {
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

func ValidateNodeType(nodeType string) error {
	switch nodeType {
	case "classic", "mtproxyl", "meko":
		return nil
	default:
		return fmt.Errorf("node_type must be classic, mtproxyl, or meko")
	}
}

// ValidateRegisterRequest — полная валидация POST /register.
func ValidateRegisterRequest(body RegisterRequest) error {
	if err := ValidateNodeID(body.NodeID); err != nil {
		return err
	}
	if err := ValidatePublicIPv4(body.IP); err != nil {
		return err
	}
	return ValidateNodeType(body.NodeType)
}

func ValidateHealthReport(payload HealthReportPayload, now time.Time) error {
	if err := ValidateNodeID(payload.NodeID); err != nil {
		return err
	}
	if err := ValidatePublicIPv4(payload.IP); err != nil {
		return err
	}
	if payload.Port < 1 || payload.Port > 65535 {
		return fmt.Errorf("port must be between 1 and 65535")
	}
	if payload.CheckedAt.IsZero() || now.Sub(payload.CheckedAt) > MaxReportAge || payload.CheckedAt.Sub(now) > MaxReportFutureSkew {
		return fmt.Errorf("checked_at is outside the accepted freshness window")
	}
	if len(payload.MetricsSnapshot) > MaxMetricsSnapshotSize {
		return fmt.Errorf("metrics_snapshot exceeds %d entries", MaxMetricsSnapshotSize)
	}
	return nil
}

// ── wire-контракты machine API (общие с агентом sharedd-node-agent) ──

// RegisterRequest — тело POST /register.
type RegisterRequest struct {
	NodeID string `json:"node_id"`
	IP     string `json:"ip"`
	// NodeType: classic/mtproxyl/meko — информационный бейдж в панели.
	NodeType string `json:"node_type,omitempty"`
}

// Intervals — тайминги циклов, раздаваемые нодам в GET /config.
type Intervals struct {
	HeartbeatMs  int `json:"heartbeat_ms"`
	GlobalpingMs int `json:"globalping_ms"`
	MetricsMs    int `json:"metrics_ms"`
	SyncMs       int `json:"sync_ms"`
}

// SharedConfigResponse — ответ GET /config.
type SharedConfigResponse struct {
	TLSDomain       string            `json:"tls_domain"`
	ProxyPort       int               `json:"proxy_port"`
	Users           map[string]string `json:"users"`
	Intervals       Intervals         `json:"intervals"`
	ForceGlobalping bool              `json:"force_globalping,omitempty"`
}

// HealthReportPayload — тело POST /report (отчёт ноды о здоровье).
type HealthReportPayload struct {
	NodeID                  string             `json:"node_id"`
	IP                      string             `json:"ip"`
	Port                    int                `json:"port"`
	FakeSNI                 string             `json:"fake_sni"`
	GlobalpingOK            bool               `json:"globalping_ok"`
	GlobalpingMeasurementID string             `json:"globalping_measurement_id"`
	GlobalpingSuccessRatio  float64            `json:"globalping_success_ratio"`
	MetricsOK               bool               `json:"metrics_ok"`
	MetricsSnapshot         map[string]float64 `json:"metrics_snapshot,omitempty"`
	Healthy                 bool               `json:"healthy"`
	CheckedAt               time.Time          `json:"checked_at"`
	Error                   string             `json:"error,omitempty"`
}
