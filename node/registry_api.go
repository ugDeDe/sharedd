package main

import (
	"io"
	"net/http"
)

// registryRequest is the single transport path for authenticated node API calls.
func registryRequest(client *http.Client, cfg *NodeConfig, method, path string, body io.Reader) (*http.Response, error) {
	req, err := http.NewRequest(method, cfg.Registry.URL+path, body)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+cfg.Registry.Token)
	if nodeID != "" {
		req.Header.Set("X-ShareDD-Node-ID", nodeID)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	return client.Do(req)
}
