package beget

// Beget API-клиент (порт mtplane/internal/providers/beget с исправлениями,
// docs/specs/beget-provisioning.md §3). Отличия от mtplane — деньги-критичные:
//   - убрана обёртка createBegetVpsWithFallback: один заказ = один POST;
//   - НЕ повторяем неидемпотентные POST при 401 (иначе потеря ответа на
//     create превратилась бы во второй платный заказ) — авто-ре-auth только
//     для GET;
//   - все вызовы принимают context с дедлайном; после отправки POST таймаут
//     возвращается как ошибка и трактуется выше как state=unknown.
// Remove при ошибке НЕ очищает id вызывающей стороной (это в provision).

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sync"
	"time"
)

// BaseURL — корень Beget API.
const BaseURL = "https://api.beget.com"

type Client struct {
	Base     string
	HC       *http.Client
	Login    string
	Password string

	mu    sync.Mutex
	token string
}

// New — клиент с таймаутом по умолчанию 60с (контекст всё равно задаёт дедлайн).
func New(login, password, cachedToken string) *Client {
	return &Client{
		Base:     BaseURL,
		HC:       &http.Client{Timeout: 60 * time.Second},
		Login:    login,
		Password: password,
		token:    cachedToken,
	}
}

func (c *Client) base() string {
	if c.Base != "" {
		return c.Base
	}
	return BaseURL
}

func (c *Client) SetToken(t string) {
	c.mu.Lock()
	c.token = t
	c.mu.Unlock()
}

func (c *Client) GetToken() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.token
}

// APIError — ответ провайдера с кодом/сообщением и HTTP-статусом. Секретов
// (login/password/token) в сообщениях нет.
type APIError struct {
	HTTPStatus  int
	Code        string
	Message     string
	Body        string
	RequestID   string
	ErrorResult string
}

func (e *APIError) Error() string {
	if e.Code != "" && e.Message != "" {
		return fmt.Sprintf("beget: %s: %s (HTTP %d)", e.Code, e.Message, e.HTTPStatus)
	}
	if e.Message != "" {
		return fmt.Sprintf("beget: %s (HTTP %d)", e.Message, e.HTTPStatus)
	}
	if e.Code != "" {
		return fmt.Sprintf("beget: %s (HTTP %d)", e.Code, e.HTTPStatus)
	}
	return fmt.Sprintf("beget: HTTP %d: %s", e.HTTPStatus, truncate(e.Body, 300))
}

// IsValidation — ошибка уровня входных данных (HTTP 4xx): повтор бессмысленен.
func (e *APIError) IsValidation() bool { return e.HTTPStatus >= 400 && e.HTTPStatus < 500 }

// Summary — безопасная для пользователя сводка: код/сообщение/статус без
// сырого тела ответа и request-id провайдера (они — внутренние детали).
func (e *APIError) Summary() string {
	switch {
	case e.Code != "" && e.Message != "":
		return fmt.Sprintf("beget: %s: %s (HTTP %d)", e.Code, e.Message, e.HTTPStatus)
	case e.Message != "":
		return fmt.Sprintf("beget: %s (HTTP %d)", e.Message, e.HTTPStatus)
	case e.Code != "":
		return fmt.Sprintf("beget: %s (HTTP %d)", e.Code, e.HTTPStatus)
	default:
		return fmt.Sprintf("beget: provider returned HTTP %d", e.HTTPStatus)
	}
}

type errorInfo struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

func parseErrRaw(raw json.RawMessage) (string, string) {
	if len(raw) == 0 || string(raw) == "null" {
		return "", ""
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		// Beget отдаёт `{"error":"INCORRECT_CREDENTIALS"}` строкой — это и есть
		// код ошибки; сохраняем его как Code, чтобы он пережил все пути
		// (APIError.Error/Summary и маппинг кабинета).
		return s, ""
	}
	var e errorInfo
	if json.Unmarshal(raw, &e) == nil {
		return e.Code, e.Message
	}
	return "", ""
}

func parseErrorBody(data []byte) (string, string) {
	var wrapper struct {
		Message string          `json:"message"`
		Error   json.RawMessage `json:"error"`
	}
	if json.Unmarshal(data, &wrapper) != nil {
		return "", ""
	}
	code, msg := parseErrRaw(wrapper.Error)
	if code == "" && msg == "" {
		msg = wrapper.Message
	}
	return code, msg
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}

func (c *Client) do(ctx context.Context, method, path string, body, out any) error {
	_, err := c.doAuthed(ctx, method, path, body, out, isIdempotent(method))
	return err
}

func (c *Client) doAuthed(ctx context.Context, method, path string, body, out any, idempotent bool) (int, error) {
	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return 0, err
		}
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.base()+path, rd)
	if err != nil {
		return 0, err
	}
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if tok := c.GetToken(); tok != "" {
		req.Header.Set("Authorization", "Bearer "+tok)
	}
	resp, err := c.HC.Do(req)
	if err != nil {
		return 0, err
	}
	defer func() { _ = resp.Body.Close() }()
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	// Авто-повтор после 401 — ТОЛЬКО для идемпотентных (GET) запросов:
	// повтор POST create после потери ответа = второй платный заказ.
	if resp.StatusCode == 401 && idempotent && c.Login != "" {
		if err := c.Auth(ctx, ""); err == nil {
			return c.doAuthed(ctx, method, path, body, out, false)
		}
	}
	if resp.StatusCode >= 400 {
		apiErr := &APIError{HTTPStatus: resp.StatusCode, Body: string(data)}
		if apiErr.RequestID = resp.Header.Get("global-request-id"); apiErr.RequestID == "" {
			apiErr.RequestID = resp.Header.Get("x-request-id")
		}
		apiErr.ErrorResult = resp.Header.Get("x-error-result")
		apiErr.Code, apiErr.Message = parseErrorBody(data)
		return resp.StatusCode, apiErr
	}
	if out != nil && len(data) > 0 {
		if err := json.Unmarshal(data, out); err != nil {
			return resp.StatusCode, fmt.Errorf("decode %s: %w (%s)", path, err, truncate(string(data), 200))
		}
	}
	return resp.StatusCode, nil
}

func isIdempotent(method string) bool {
	return method == http.MethodGet || method == http.MethodHead || method == http.MethodDelete
}

type authRequest struct {
	Login    string `json:"login"`
	Password string `json:"password"`
	SaveMe   bool   `json:"saveMe"`
	// Code — необязательный 2FA/verification код. omitempty: пустой → поле
	// отсутствует в теле (провайдер не ждёт code, когда 2FA выключена).
	Code string `json:"code,omitempty"`
}

type authResponse struct {
	Token string          `json:"token"`
	Error json.RawMessage `json:"error"`
}

// Auth — вход в личный кабинет; полученный токен кэшируется в клиенте.
// code — необязательный 2FA/verification код (пустой = не отправлять). Ошибку
// провайдера возвращаем как *APIError с его Code/Message (включая строковую
// форму `{"error":"CODE"}`), чтобы вызывающий мог смаппить причину.
func (c *Client) Auth(ctx context.Context, code string) error {
	req := authRequest{Login: c.Login, Password: c.Password, SaveMe: true, Code: code}
	var resp authResponse
	status, err := c.doAuthed(ctx, http.MethodPost, "/v1/auth", req, &resp, false)
	if err != nil {
		return err
	}
	if len(resp.Error) > 0 && string(resp.Error) != "null" {
		errCode, msg := parseErrRaw(resp.Error)
		if errCode == "" && msg == "" {
			msg = "auth error"
		}
		return &APIError{HTTPStatus: status, Code: errCode, Message: msg}
	}
	if resp.Token == "" {
		return errors.New("beget: empty token")
	}
	c.SetToken(resp.Token)
	return nil
}

// VpsInfo — карточка сервера.
type VpsInfo struct {
	ID          string `json:"id"`
	Slug        string `json:"slug"`
	DisplayName string `json:"display_name"`
	Hostname    string `json:"hostname"`
	Status      string `json:"status"`
	IPAddress   string `json:"ip_address"`
	DateCreate  string `json:"date_create"`
	Region      string `json:"region,omitempty"`
	HasPassword bool   `json:"has_password"`
}

// CreateVpsRequest — тело заказа сервера. Валидация — до вызова.
type CreateVpsRequest struct {
	DisplayName         string         `json:"display_name,omitempty"`
	Hostname            string         `json:"hostname,omitempty"`
	Description         string         `json:"description,omitempty"`
	ConfigurationID     string         `json:"configuration_id,omitempty"`
	ConfigurationParams map[string]any `json:"configuration_params,omitempty"`
	ImageID             string         `json:"image_id,omitempty"`
	SSHKeys             []uint32       `json:"ssh_keys,omitempty"`
	Region              string         `json:"region,omitempty"`
	LinkSlug            string         `json:"link_slug,omitempty"`
}

type CreateVpsResponse struct {
	Vps   *VpsInfo   `json:"vps"`
	Error *errorInfo `json:"error"`
}

// CreateVps — РОВНО ОДИН POST на заказ. Никаких fallback-повторов.
func (c *Client) CreateVps(ctx context.Context, req CreateVpsRequest) (*CreateVpsResponse, error) {
	var out CreateVpsResponse
	status, err := c.doAuthed(ctx, http.MethodPost, "/v1/vps/server", req, &out, false)
	if err != nil {
		return nil, err
	}
	if out.Error != nil && out.Vps == nil {
		return &out, &APIError{HTTPStatus: status, Code: out.Error.Code, Message: out.Error.Message}
	}
	return &out, nil
}

// GetListResponse — список серверов (для сверки unknown-операций).
type GetListResponse struct {
	Vps        []VpsInfo `json:"vps"`
	TotalCount int       `json:"total_count"`
}

func (c *Client) GetList(ctx context.Context) (*GetListResponse, error) {
	var out GetListResponse
	if err := c.do(ctx, http.MethodGet, "/v1/vps/server/list?limit=9999", nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

type GetInfoResponse struct {
	Vps *VpsInfo `json:"vps"`
}

func (c *Client) GetInfo(ctx context.Context, id string) (*GetInfoResponse, error) {
	var out GetInfoResponse
	if err := c.do(ctx, http.MethodGet, "/v1/vps/server/"+id, nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// RegionInfo — регион из каталога (валидация до заказа).
type RegionInfo struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	NameEn    string `json:"name_en"`
	Country   string `json:"country"`
	Available bool   `json:"available"`
}

func (c *Client) GetRegions(ctx context.Context) ([]RegionInfo, error) {
	var out struct {
		Regions []RegionInfo `json:"regions"`
	}
	if err := c.do(ctx, http.MethodGet, "/v1/vps/region", nil, &out); err != nil {
		return nil, err
	}
	return out.Regions, nil
}

// ConfigurationInfo — тариф из каталога (addendum-profiles-vps.md §4.1):
// id/название/cpu/ram/disk и цена за период. Price — *float64: провайдер
// может не отдавать цену → null в JSON (НЕ 0, которая соврала бы).
type ConfigurationInfo struct {
	ID        string   `json:"id"`
	Name      string   `json:"name"`
	CPU       int      `json:"cpu,omitempty"`
	RAM       int      `json:"ram,omitempty"`  // МБ
	Disk      int      `json:"disk,omitempty"` // МБ
	Price     *float64 `json:"price"`          // за период; null если провайдер опустил
	Period    string   `json:"period,omitempty"`
	Region    string   `json:"region,omitempty"`
	Available bool     `json:"available"`
}

// GetConfigurations — каталог тарифов. Регион в запросе необязателен: клиент
// вызывающего сам решает, фильтровать ли. Валидация тарифа — ДО платного вызова.
func (c *Client) GetConfigurations(ctx context.Context) ([]ConfigurationInfo, error) {
	var out struct {
		Configurations []ConfigurationInfo `json:"configurations"`
	}
	if err := c.do(ctx, http.MethodGet, "/v1/vps/configuration", nil, &out); err != nil {
		return nil, err
	}
	return out.Configurations, nil
}

// SSHKey — ключ, привязанный к аккаунту.
type SSHKey struct {
	ID          uint32   `json:"id"`
	Name        string   `json:"name"`
	Fingerprint string   `json:"fingerprint"`
	VpsID       []string `json:"vps_id"`
}

func (c *Client) ListSSHKeys(ctx context.Context) ([]SSHKey, error) {
	var out struct {
		Keys []SSHKey `json:"keys"`
	}
	if err := c.do(ctx, http.MethodGet, "/v1/vps/sshKey", nil, &out); err != nil {
		return nil, err
	}
	return out.Keys, nil
}

func (c *Client) AddSSHKey(ctx context.Context, name, pubKey string) (*SSHKey, error) {
	var out struct {
		Key   *SSHKey    `json:"key"`
		Error *errorInfo `json:"error"`
	}
	status, err := c.doAuthed(ctx, http.MethodPost, "/v1/vps/sshKey", map[string]any{"name": name, "public_key": pubKey}, &out, false)
	if err != nil {
		return nil, err
	}
	if out.Error != nil {
		return nil, &APIError{HTTPStatus: status, Code: out.Error.Code, Message: out.Error.Message}
	}
	return out.Key, nil
}

// Remove — удаление сервера. Ошибка НЕ означает удаление: вызывающий обязан
// сохранить provider_vps_id и оставить state=deleting до подтверждения.
func (c *Client) Remove(ctx context.Context, id string) error {
	var out struct {
		Error *errorInfo `json:"error"`
	}
	status, err := c.doAuthed(ctx, http.MethodPost, "/v1/vps/server/"+id+"/remove", map[string]any{}, &out, false)
	if err != nil {
		return err
	}
	if out.Error != nil {
		return &APIError{HTTPStatus: status, Code: out.Error.Code, Message: out.Error.Message}
	}
	return nil
}
