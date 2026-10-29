// Package nodehost provides bounded Nodehost API calls, not lifecycle orchestration.
package nodehost

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

const BaseURL = "https://nodehost.cc/api"
const maxBody = 1 << 20
const requestTimeout = 15 * time.Second

// Error contains only locally selected codes, never provider text or payloads.
// A mutation error is not evidence that no remote side effect occurred.
type Error struct {
	Code       string
	HTTPStatus int
	cause      error
}

func (e *Error) Error() string  { return "nodehost: " + e.Code }
func (e *Error) Unwrap() error  { return e.cause }
func failure(code string) error { return &Error{Code: code} }

type Client struct {
	key  string
	base string // injectable only within the package's local HTTP tests
	hc   *http.Client
}

func New(apiKey string) (*Client, error) {
	if !text(apiKey, 4096) || strings.ContainsAny(apiKey, " \t") {
		return nil, failure("credentials_invalid")
	}
	return &Client{key: apiKey, base: BaseURL, hc: &http.Client{Timeout: requestTimeout}}, nil
}

func text(s string, max int) bool {
	return strings.TrimSpace(s) != "" && utf8.ValidString(s) && utf8.RuneCountInString(s) <= max && !strings.ContainsFunc(s, unicode.IsControl)
}

func validID(id string) bool { return text(id, 256) && id != "." && id != ".." }

// boundCode maps codes/messages to fixed local values. Truncation or stripping
// control characters is not redaction: even a short provider value can be a key.
func boundCode(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	switch s {
	case "insufficient_funds", "validation_failed", "unauthorized", "forbidden",
		"not_found", "conflict", "rate_limited", "provider_error":
		return s
	}
	if strings.Contains(s, "insufficient funds") || strings.Contains(s, "insufficient balance") ||
		strings.Contains(s, "недостаточно средств") {
		return "insufficient_funds"
	}
	return "provider_error"
}

func transportError(ctx context.Context, err error) error {
	cause := ctx.Err()
	if cause == nil && errors.Is(err, context.DeadlineExceeded) {
		cause = context.DeadlineExceeded
	}
	code := "transport_error"
	if cause != nil {
		code = "request_canceled"
	}
	return &Error{Code: code, cause: cause}
}

func safeProviderError(status int) *Error {
	code := "provider_error"
	switch status {
	case 400:
		code = "validation_failed"
	case 401:
		code = "unauthorized"
	case 402:
		code = "insufficient_funds"
	case 403:
		code = "forbidden"
	case 404:
		code = "not_found"
	case 409:
		code = "conflict"
	case 429:
		code = "rate_limited"
	}
	return &Error{Code: code, HTTPStatus: status}
}

func (c *Client) do(ctx context.Context, method, path, key string, body, out any, expected ...int) error {
	return c.doDecode(ctx, method, path, key, body, out, expected, true)
}

// doDecode performs the request and decodes the body into out. The generic
// business-error envelope check ({"error":{...}} at expected status) runs only
// when envelope is true — Operation skips it because a failed operation
// legitimately carries an error object inside its 200 body.
func (c *Client) doDecode(ctx context.Context, method, path, key string, body, out any, expected []int, envelope bool) error {
	ctx, cancel := context.WithTimeout(ctx, requestTimeout)
	defer cancel()
	var rd io.Reader
	if body != nil {
		data, err := json.Marshal(body)
		if err != nil {
			return failure("invalid_input")
		}
		rd = bytes.NewReader(data)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.base+path, rd)
	if err != nil {
		return failure("invalid_request")
	}
	req.Header.Set("Authorization", "Bearer "+c.key)
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if key != "" {
		req.Header.Set("Idempotency-Key", key)
	}
	// net/http considers Idempotency-Key replayable. Remove GetBody so even a
	// reused-connection transport failure cannot transparently replay this POST.
	req.GetBody = nil
	hc := *c.hc
	hc.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	resp, err := hc.Do(req)
	if err != nil {
		return transportError(ctx, err)
	}
	defer func() { _ = resp.Body.Close() }()
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxBody+1))
	if err != nil {
		return transportError(ctx, err)
	}
	if len(data) > maxBody {
		return failure("response_too_large")
	}
	// Status mapping first: non-JSON error bodies (HTML/plain text) must keep
	// their status-mapped code instead of collapsing into invalid_json.
	statusOK := false
	for _, s := range expected {
		if resp.StatusCode == s {
			statusOK = true
			break
		}
	}
	if !statusOK {
		return safeProviderError(resp.StatusCode)
	}
	if len(data) > 0 && !json.Valid(data) {
		return failure("invalid_json")
	}
	if envelope {
		var env struct {
			Error json.RawMessage `json:"error"`
		}
		// Arrays are legitimate catalog responses; objects may contain business errors.
		if len(bytes.TrimSpace(data)) > 0 && bytes.TrimSpace(data)[0] == '{' {
			if json.Unmarshal(data, &env) != nil {
				return failure("invalid_response")
			}
			if len(env.Error) != 0 && string(env.Error) != "null" {
				return safeProviderError(resp.StatusCode)
			}
		}
	}
	if bytes.Equal(bytes.TrimSpace(data), []byte("null")) || json.Unmarshal(data, out) != nil {
		return failure("invalid_response")
	}
	return nil
}
