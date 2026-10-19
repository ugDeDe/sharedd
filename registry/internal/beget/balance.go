package beget

// Отдельный клиент баланса API хостинга Beget
// (docs/specs/hosting-blueprints/02-provider-api.md §4). Это НЕ cloud JWT API
// и не APIError cloud-клиента: hosting login/passwd хранятся отдельно, в
// ошибки и логи не попадают. Валюта на wire — null; applicability к VPS
// UNCONFIRMED (scope beget_hosting_unverified решает вызывающий слой).

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"
)

const maxBalanceBody = 1 << 20

// BalanceClient — самостоятельный клиент: отдельные credentials, отдельный
// ответ конверта, ничего общего с токеном cloud-клиента.
type BalanceClient struct {
	Base        string
	HC          *http.Client
	Login       string
	APIPassword string
}

// NewBalanceClient — 15s таймаут по умолчанию (контекст всё равно задаёт дедлайн).
func NewBalanceClient(login, apiPassword string) *BalanceClient {
	return &BalanceClient{
		HC:          &http.Client{Timeout: 15 * time.Second},
		Login:       login,
		APIPassword: apiPassword,
	}
}

func (c *BalanceClient) base() string {
	if c.Base != "" {
		return c.Base
	}
	return BaseURL
}

// scrubMessage маскирует credentials, если провайдер эхом вернул их в
// error_text (частый случай для auth-ошибок). ponytail: маски только ≥3
// символов — короче замаскируют весь текст впустую; осмысленного echo
// credentials короче 3 символов не бывает.
func (c *BalanceClient) scrubMessage(msg string) string {
	if len(c.Login) >= 3 {
		msg = strings.ReplaceAll(msg, c.Login, "***")
	}
	if len(c.APIPassword) >= 3 {
		msg = strings.ReplaceAll(msg, c.APIPassword, "***")
	}
	return msg
}

// BalanceError — безопасная ошибка: код/текст от провайдера ограничены и
// очищены; credentials никогда не включаются.
type BalanceError struct {
	Code       string
	Message    string
	HTTPStatus int
}

func (e *BalanceError) Error() string {
	return "beget balance: " + e.Code
}

// Balance — нормализованный снимок баланса хостинг-API. Currency пуст при
// null; Amount — точная десятичная строка, без float64.
type Balance struct {
	Amount   string
	Currency string
	Scope    string
}

type balanceEnvelope struct {
	Status    string `json:"status"`
	ErrorCode string `json:"error_code"`
	ErrorText string `json:"error_text"`
	Answer    *struct {
		Status string `json:"status"`
		Errors []struct {
			Code string `json:"error_code"`
			Text string `json:"error_text"`
		} `json:"errors"`
		Result struct {
			UserBalance json.RawMessage `json:"user_balance"`
		} `json:"result"`
	} `json:"answer"`
}

var balanceAmountPattern = regexp.MustCompile(`^-?[0-9]+(\.[0-9]+)?([eE][+-]?[0-9]+)?$`)

// sanitizeBound очищает провайдерский текст: control-символы выкидываются,
// длина ограничена. Ошибка никогда не содержит credentials — сюда попадает
// только отквоченный текст ответа провайдера.
func sanitizeBound(s string) string {
	s = strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f {
			return -1
		}
		return r
	}, s)
	if len(s) > 200 {
		s = string([]rune(s)[:200])
	}
	return strings.TrimSpace(s)
}

// normalizeAmount приводит JSON-число или строку к канонической десятичной
// строке без float64 (тот же паттерн, что nodehost.Decimal). Пустое или
// нечисловое значение — ошибка, не 0.
func normalizeAmount(data json.RawMessage) (string, bool) {
	s := string(data)
	if len(data) > 0 && data[0] == '"' {
		if json.Unmarshal(data, &s) != nil {
			return "", false
		}
	}
	if s == "" || len(s) > 128 || !balanceAmountPattern.MatchString(s) {
		return "", false
	}
	mant, exp := s, 0
	if i := strings.IndexAny(s, "eE"); i >= 0 {
		e, err := strconv.Atoi(s[i+1:])
		if err != nil || e < -128 || e > 128 {
			return "", false
		}
		mant, exp = s[:i], e
	}
	sign := ""
	if strings.HasPrefix(mant, "-") {
		sign, mant = "-", mant[1:]
	}
	point := strings.IndexByte(mant, '.')
	if point < 0 {
		point = len(mant)
	}
	digits := strings.ReplaceAll(mant, ".", "")
	point += exp
	switch {
	case point <= 0:
		s = "0." + strings.Repeat("0", -point) + digits
	case point >= len(digits):
		s = digits + strings.Repeat("0", point-len(digits))
	default:
		s = digits[:point] + "." + digits[point:]
	}
	return sign + s, true
}

// Balance запрашивает баланс хостинг-API: POST form-urlencoded в теле,
// credentials не переносятся в query URL. Двухуровневый конверт: успешен
// только status==success И answer.status==success.
func (c *BalanceClient) Balance(ctx context.Context) (*Balance, error) {
	if c.Login == "" || c.APIPassword == "" {
		return nil, &BalanceError{Code: "balance_credentials_missing"}
	}
	var cancel context.CancelFunc
	ctx, cancel = context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	form := url.Values{}
	form.Set("login", c.Login)
	form.Set("passwd", c.APIPassword)
	form.Set("output_format", "json")
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.base()+"/api/user/getAccountInfo", strings.NewReader(form.Encode()))
	if err != nil {
		return nil, &BalanceError{Code: "invalid_request"}
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	// Redirects denied per call (mirror nodehost client.go): тело несёт
	// login+passwd, 307/308 переслал бы их на Location-цель.
	hc := *c.HC
	hc.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	resp, err := hc.Do(req)
	if err != nil {
		return nil, &BalanceError{Code: "transport_error"}
	}
	defer func() { _ = resp.Body.Close() }()
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxBalanceBody+1))
	if err != nil {
		return nil, &BalanceError{Code: "transport_error"}
	}
	if len(data) > maxBalanceBody {
		return nil, &BalanceError{Code: "response_too_large"}
	}
	var env balanceEnvelope
	if json.Unmarshal(data, &env) != nil {
		return nil, &BalanceError{Code: "invalid_response", HTTPStatus: resp.StatusCode}
	}
	if env.Status != "success" {
		if env.Status == "error" {
			code := sanitizeBound(env.ErrorCode)
			if code == "" {
				code = "provider_error"
			}
			return nil, &BalanceError{Code: code, Message: c.scrubMessage(sanitizeBound(env.ErrorText)), HTTPStatus: resp.StatusCode}
		}
		return nil, &BalanceError{Code: "invalid_response", HTTPStatus: resp.StatusCode}
	}
	if env.Answer == nil || env.Answer.Status != "success" {
		if env.Answer != nil && env.Answer.Status == "error" && len(env.Answer.Errors) > 0 {
			code := sanitizeBound(env.Answer.Errors[0].Code)
			if code == "" {
				code = "provider_error"
			}
			return nil, &BalanceError{Code: code, Message: c.scrubMessage(sanitizeBound(env.Answer.Errors[0].Text)), HTTPStatus: resp.StatusCode}
		}
		return nil, &BalanceError{Code: "invalid_response", HTTPStatus: resp.StatusCode}
	}
	amount, ok := normalizeAmount(env.Answer.Result.UserBalance)
	if !ok {
		return nil, &BalanceError{Code: "invalid_balance", HTTPStatus: resp.StatusCode}
	}
	return &Balance{Amount: amount, Scope: "beget_hosting_unverified"}, nil
}
