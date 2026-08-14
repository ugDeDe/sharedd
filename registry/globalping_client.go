package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"
)

type globalpingProbeResult struct {
	Status     string `json:"status"`
	StatusCode int    `json:"statusCode"`
}

// globalpingProbeInfo — площадка, откуда выполнялась проба (V7.7: показываем
// на странице ноды, откуда именно светит/не светит прокси).
type globalpingProbeInfo struct {
	Continent string `json:"continent"`
	Country   string `json:"country"`
	City      string `json:"city"`
	Network   string `json:"network"`
	ASN       int    `json:"asn"`
}

type globalpingProbeMeasurement struct {
	Probe  globalpingProbeInfo   `json:"probe"`
	Result globalpingProbeResult `json:"result"`
}

type globalpingMeasurement struct {
	ID      string                       `json:"id"`
	Status  string                       `json:"status"`
	Results []globalpingProbeMeasurement `json:"results"`
}

type GlobalpingChecker struct {
	APIBase string
	Client  *http.Client
}

// NewGlobalpingChecker — клиент ТОЛЬКО для скачивания готовых measurement'ов
// (GET /measurements/{id}) — независимая верификация отчётов нод.
// Токена здесь нет сознательно: hourly-квоты Globalping (250/500 тестов в час)
// относятся к СОЗДАНИЮ measurement'ов (это делают ноды анонимно со своих IP),
// а на GET действует лишь burst-лимит 2 req/s на measurement — при нашем
// темпе (1 GET на GP-отчёт ноды) он недостижим, и токен его не поднимает.
func NewGlobalpingChecker(apiBase string) *GlobalpingChecker {
	if apiBase == "" {
		apiBase = "https://api.globalping.io/v1"
	}
	return &GlobalpingChecker{
		APIBase: apiBase,
		Client:  &http.Client{Timeout: 15 * time.Second},
	}
}

func (g *GlobalpingChecker) FetchMeasurement(id string) (*globalpingMeasurement, error) {
	req, err := http.NewRequest(http.MethodGet, g.APIBase+"/measurements/"+id, nil)
	if err != nil {
		return nil, err
	}

	resp, err := g.Client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("globalping fetch error: %w", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("globalping fetch failed: status=%d body=%s", resp.StatusCode, string(body))
	}
	var m globalpingMeasurement
	if err := json.Unmarshal(body, &m); err != nil {
		return nil, fmt.Errorf("globalping fetch parse error: %w", err)
	}
	return &m, nil
}

// FetchFinished (V7.9.1) — скачать measurement, дождавшись его завершения.
// Нода присылает отчёт сразу после своего ожидания, но если оно у неё
// вышло по таймауту, measurement ещё может быть in-progress: оценивать такой
// по частичным результатам нельзя — недосчитанные пробы и давали «globalping
// пишет 0». Поллим раз в 2 с (burst-лимит API — 2 req/s на measurement, нам
// хватает). По таймауту возвращаем ПОСЛЕДНИЙ снапшот + ошибку — вызывающий
// код решает, считать ли верификацию несостоявшейся.
func (g *GlobalpingChecker) FetchFinished(id string, timeout time.Duration) (*globalpingMeasurement, error) {
	deadline := time.Now().Add(timeout)
	for {
		m, err := g.FetchMeasurement(id)
		if err != nil {
			return nil, err
		}
		// по OpenAPI top-level статус — только in-progress/finished
		if m.Status != "in-progress" {
			return m, nil
		}
		if time.Now().After(deadline) {
			return m, fmt.Errorf("measurement %s still in-progress after %s", id, timeout)
		}
		time.Sleep(2 * time.Second)
	}
}

// probeResultOK — проба успешна: измерение завершилось ответом 2xx/3xx.
func probeResultOK(r globalpingProbeMeasurement) bool {
	return r.Result.Status == "finished" && r.Result.StatusCode >= 200 && r.Result.StatusCode < 400
}

func evaluateSuccessRatio(m *globalpingMeasurement) float64 {
	if m == nil || len(m.Results) == 0 {
		return 0
	}
	success := 0
	for _, r := range m.Results {
		if probeResultOK(r) {
			success++
		}
	}
	return float64(success) / float64(len(m.Results))
}
