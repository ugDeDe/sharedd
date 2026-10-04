package server

import (
	"crypto/sha256"
	"crypto/subtle"
	"net/http"
	"strings"

	"sharedd/registry/internal/machineapi"
)

// Bearer-аутентификация machine API. Сами контракты и валидация тел —
// в internal/machineapi; здесь только привязка к Registry.

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

var (
	validateNodeID          = machineapi.ValidateNodeID
	validatePublicIPv4      = machineapi.ValidatePublicIPv4
	validateRegisterRequest = machineapi.ValidateRegisterRequest
	validateHealthReport    = machineapi.ValidateHealthReport
	decodeNodeJSON          = machineapi.DecodeJSON
)

// Алиасы wire-контрактов: сервер и тесты используют исторические имена.
type (
	registerRequest      = machineapi.RegisterRequest
	nodeIntervals        = machineapi.Intervals
	sharedConfigResponse = machineapi.SharedConfigResponse
	HealthReportPayload  = machineapi.HealthReportPayload
)

const (
	maxNodeJSONBytes       = machineapi.MaxNodeJSONBytes
	maxMetricsSnapshotSize = machineapi.MaxMetricsSnapshotSize
	maxReportAge           = machineapi.MaxReportAge
	maxReportFutureSkew    = machineapi.MaxReportFutureSkew
)
