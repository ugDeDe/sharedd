// Package checkhost — клиент check-host.net (https://check-host.net/about/api)
// для TCP-проверки: список узлов, запуск проверки с выбранных узлов и
// результат по каждому.
package checkhost

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"
)

// DefaultAPIBase — база API по умолчанию.
const DefaultAPIBase = "https://check-host.net"

// Node — узел проверки: имя (ru1.node.check-host.net) и страна (ISO, нижний регистр).
type Node struct {
	Name    string
	Country string
}

// TCPResult — результат одного узла: Pending — ещё идёт; иначе OK или Error.
type TCPResult struct {
	Pending bool
	OK      bool
	Error   string
	TimeSec float64
}

// Client — база и HTTP-клиент.
type Client struct {
	APIBase string
	HTTP    *http.Client
}

// New — клиент с таймаутом запроса.
func New(apiBase string) *Client {
	if strings.TrimSpace(apiBase) == "" {
		apiBase = DefaultAPIBase
	}
	return &Client{APIBase: strings.TrimRight(apiBase, "/"), HTTP: &http.Client{Timeout: 20 * time.Second}}
}

func (c *Client) get(ctx context.Context, path string, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.APIBase+path, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "sharedd-registry (+https://github.com/ugDeDe/sharedd)")
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return err
	}
	if resp.StatusCode != http.StatusOK {
		msg := strings.TrimSpace(string(body))
		if len(msg) > 200 {
			msg = msg[:200]
		}
		return fmt.Errorf("check-host %s: HTTP %d: %s", path, resp.StatusCode, msg)
	}
	if err := json.Unmarshal(body, out); err != nil {
		return fmt.Errorf("check-host %s: bad JSON: %w", path, err)
	}
	return nil
}

// Nodes — все узлы проверки (GET /nodes/hosts), по имени.
func (c *Client) Nodes(ctx context.Context) ([]Node, error) {
	var resp struct {
		Nodes map[string]struct {
			Location []string `json:"location"`
		} `json:"nodes"`
	}
	if err := c.get(ctx, "/nodes/hosts", &resp); err != nil {
		return nil, err
	}
	out := make([]Node, 0, len(resp.Nodes))
	for name, n := range resp.Nodes {
		country := ""
		if len(n.Location) > 0 {
			country = strings.ToLower(n.Location[0])
		}
		out = append(out, Node{Name: name, Country: country})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// StartTCP — TCP-проверка host:port с узлов nodes (GET /check-tcp); id запроса.
func (c *Client) StartTCP(ctx context.Context, host string, port int, nodes []string) (string, error) {
	q := url.Values{}
	q.Set("host", fmt.Sprintf("%s:%d", host, port))
	for _, n := range nodes {
		q.Add("node", n)
	}
	var resp struct {
		OK        int    `json:"ok"`
		RequestID string `json:"request_id"`
	}
	if err := c.get(ctx, "/check-tcp?"+q.Encode(), &resp); err != nil {
		return "", err
	}
	if resp.OK != 1 || resp.RequestID == "" {
		return "", fmt.Errorf("check-host: check not accepted")
	}
	return resp.RequestID, nil
}

// Result — результат TCP-проверки по узлам (GET /check-result/<id>).
func (c *Client) Result(ctx context.Context, id string) (map[string]TCPResult, error) {
	var raw map[string]json.RawMessage
	if err := c.get(ctx, "/check-result/"+url.PathEscape(id), &raw); err != nil {
		return nil, err
	}
	out := make(map[string]TCPResult, len(raw))
	for node, msg := range raw {
		out[node] = parseTCP(msg)
	}
	return out, nil
}

// parseTCP — null: идёт; [{"time":…}]: успех; [{"error":…}]: отказ.
func parseTCP(msg json.RawMessage) TCPResult {
	if len(msg) == 0 || string(msg) == "null" {
		return TCPResult{Pending: true}
	}
	var items []*struct {
		Time  *float64 `json:"time"`
		Error string   `json:"error"`
	}
	if err := json.Unmarshal(msg, &items); err != nil || len(items) == 0 || items[0] == nil {
		return TCPResult{Error: "unparsed result"}
	}
	it := items[0]
	if it.Error != "" {
		return TCPResult{Error: it.Error}
	}
	if it.Time != nil {
		return TCPResult{OK: true, TimeSec: *it.Time}
	}
	return TCPResult{Error: "no time and no error"}
}
