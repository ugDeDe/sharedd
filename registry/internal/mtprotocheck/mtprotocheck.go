// Package mtprotocheck — клиент внешней MTProto-проверки прокси
// (POST /v1/check): настоящий fake-TLS/obf2 хендшейк и запрос до DC из РФ
// (за ТСПУ) и снаружи. Регистратор зовёт её вместе с верификацией
// Globalping; ключ — [mtproto_check] token.
package mtprotocheck

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// DefaultAPIBase — база API по умолчанию.
const DefaultAPIBase = "https://api.yazaebalsyadelatproxy.cc"

// Статусы ответа.
const (
	StatusOK       = "OK"        // данные до DC доходят и из РФ, и снаружи
	StatusDataCut  = "DATA_CUT"  // из РФ хендшейк есть, поток до DC режется
	StatusRKNBlock = "RKN_BLOCK" // из РФ не проходит хендшейк/TCP
	StatusMTDead   = "MT_DEAD"   // снаружи поток до DC не доходит при живом TCP
	StatusDown     = "DOWN"      // сервер недоступен полностью
	StatusUnknown  = "UNKNOWN"   // нестандартная картина
)

// Point — результат одной точки обзора.
type Point struct {
	Label      string   `json:"label"`
	Handshake  *bool    `json:"handshake"`
	Data       *bool    `json:"data"`
	TCP        *bool    `json:"tcp"`
	LatencyMs  *float64 `json:"latency_ms"`
	DurationMs *float64 `json:"duration_ms"`
}

// Result — ответ POST /v1/check.
type Result struct {
	Status     string `json:"status"`
	Server     string `json:"server"`
	Port       int    `json:"port"`
	SecretType string `json:"secret_type"`
	CheckedAt  string `json:"checked_at"`
	Local      *Point `json:"local"`
	Remote     *Point `json:"remote"`
}

// Verdict — однозначен ли результат и доступен ли прокси из РФ. Решает
// только TCP с российской точки (local.tcp): false — недоступен, true —
// доступен; статус сервиса не учитывается (MT_DEAD, например, mtdd получает
// всегда: сервис просит DC вне 1..5, а RKN_BLOCK не ставится, когда TCP не
// открылся и снаружи). Нет точки или поля tcp — неоднозначно.
func (r *Result) Verdict() (ok, conclusive bool) {
	if r == nil || r.Local == nil || r.Local.TCP == nil {
		return false, false
	}
	return *r.Local.TCP, true
}

// Client — ключ, база и HTTP-клиент.
type Client struct {
	APIBase string
	Token   string
	HTTP    *http.Client
}

// New — клиент с запасом на проверку до 15 с плюс сеть.
func New(apiBase, token string) *Client {
	if strings.TrimSpace(apiBase) == "" {
		apiBase = DefaultAPIBase
	}
	return &Client{APIBase: strings.TrimRight(apiBase, "/"), Token: token, HTTP: &http.Client{Timeout: 40 * time.Second}}
}

// Check — синхронная проверка server:port с секретом (hex, ee… для
// fake-TLS); timeoutSec зажимается сервисом в 1..15.
func (c *Client) Check(ctx context.Context, server string, port int, secret string, timeoutSec int) (*Result, error) {
	body, _ := json.Marshal(map[string]any{"server": server, "port": port, "secret": secret})
	url := fmt.Sprintf("%s/v1/check?timeout=%d", c.APIBase, timeoutSec)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-API-Key", c.Token)
	req.Header.Set("User-Agent", "sharedd-registry (+https://github.com/ugDeDe/sharedd)")
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		msg := strings.TrimSpace(string(raw))
		if len(msg) > 200 {
			msg = msg[:200]
		}
		if ra := resp.Header.Get("Retry-After"); ra != "" {
			msg += " (Retry-After " + ra + ")"
		}
		return nil, fmt.Errorf("mtproto check: HTTP %d: %s", resp.StatusCode, msg)
	}
	var res Result
	if err := json.Unmarshal(raw, &res); err != nil {
		return nil, fmt.Errorf("mtproto check: bad JSON: %w", err)
	}
	if res.Status == "" {
		return nil, fmt.Errorf("mtproto check: response without status")
	}
	return &res, nil
}

// Summary — короткая строка для журнала: статус и хендшейк/данные по точкам.
func (r *Result) Summary() string {
	if r == nil {
		return ""
	}
	pt := func(name string, p *Point) string {
		if p == nil {
			return name + " —"
		}
		b := func(v *bool) string {
			switch {
			case v == nil:
				return "?"
			case *v:
				return "+"
			}
			return "-"
		}
		return fmt.Sprintf("%s tcp%s hs%s data%s", name, b(p.TCP), b(p.Handshake), b(p.Data))
	}
	return fmt.Sprintf("%s (%s; %s)", r.Status, pt("RU", r.Local), pt("NL", r.Remote))
}
