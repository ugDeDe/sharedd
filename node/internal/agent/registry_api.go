package agent

import (
	"io"
	"net/http"

	"sharedd/node/internal/config"
)

// registryRequest is the single transport path for authenticated node API calls.
// Bearer: персональный node_secret предпочтительнее общего токена
// (invitations-migration-contract.md §1) — реестр различает их по хешу.
func registryRequest(client *http.Client, cfg *config.NodeConfig, method, path string, body io.Reader) (*http.Response, error) {
	req, err := http.NewRequest(method, cfg.Registry.URL+path, body)
	if err != nil {
		return nil, err
	}
	bearer := cfg.Registry.Token
	if nodeSecret != "" {
		bearer = nodeSecret
	}
	req.Header.Set("Authorization", "Bearer "+bearer)
	if nodeID != "" {
		req.Header.Set("X-ShareDD-Node-ID", nodeID)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	return client.Do(req)
}
