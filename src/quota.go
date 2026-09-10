package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

const (
	quotaEndpoint       = "https://api.z.ai/api/monitor/usage/quota/limit"
	maxQuotaResponse    = 1 << 20
	defaultQuotaTimeout = 15 * time.Second
)

type httpDoer interface {
	Do(*http.Request) (*http.Response, error)
}

type quotaWindow struct {
	ConsumedMicrocredits int64
	BucketMicrocredits   int64
	ResetsAt             time.Time
}

type quotaSnapshot struct {
	Plan       string
	FiveHour   quotaWindow
	Weekly     quotaWindow
	ObservedAt time.Time
}

type quotaWireResponse struct {
	Code    json.Number   `json:"code"`
	Success *bool         `json:"success"`
	Data    quotaWireData `json:"data"`
}

type quotaWireData struct {
	Level    string           `json:"level"`
	PlanName string           `json:"planName"`
	Limits   []quotaWireLimit `json:"limits"`
}

type quotaWireLimit struct {
	Type          string      `json:"type"`
	Unit          json.Number `json:"unit"`
	Number        json.Number `json:"number"`
	Usage         json.Number `json:"usage"`
	CurrentValue  json.Number `json:"currentValue"`
	Remaining     json.Number `json:"remaining"`
	NextResetTime json.Number `json:"nextResetTime"`
}

func newQuotaHTTPClient(timeout time.Duration) *http.Client {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.Proxy = nil
	client := &http.Client{Transport: transport, Timeout: quotaTimeout(timeout)}
	client.CheckRedirect = func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}
	return client
}

func fetchQuota(ctx context.Context, client httpDoer, endpoint, key string, now time.Time) (quotaSnapshot, error) {
	if client == nil {
		return quotaSnapshot{}, fmt.Errorf("quota client unavailable")
	}
	if err := validateQuotaEndpoint(endpoint, endpoint != quotaEndpoint); err != nil {
		return quotaSnapshot{}, err
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return quotaSnapshot{}, fmt.Errorf("create quota request")
	}
	request.Header.Set("Authorization", "Bearer "+key)
	request.Header.Set("Accept", "application/json")
	response, err := client.Do(request)
	if err != nil {
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return quotaSnapshot{}, err
		}
		return quotaSnapshot{}, fmt.Errorf("quota request failed")
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode != http.StatusOK {
		return quotaSnapshot{}, fmt.Errorf("quota endpoint returned status %d", response.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(response.Body, maxQuotaResponse+1))
	if err != nil {
		return quotaSnapshot{}, fmt.Errorf("read quota response")
	}
	if len(body) > maxQuotaResponse {
		return quotaSnapshot{}, fmt.Errorf("quota response exceeds maximum size")
	}
	snapshot, err := parseQuotaResponse(body, now)
	if err != nil {
		return quotaSnapshot{}, fmt.Errorf("invalid quota response: %w", err)
	}
	return snapshot, nil
}

func validateQuotaEndpoint(endpoint string, allowTestEndpoint bool) error {
	parsed, err := url.Parse(endpoint)
	if err != nil || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" {
		return fmt.Errorf("invalid quota endpoint")
	}
	if allowTestEndpoint && parsed.Scheme == "http" && (parsed.Hostname() == "127.0.0.1" || parsed.Hostname() == "localhost") {
		return nil
	}
	if parsed.Scheme != "https" || parsed.Host != "api.z.ai" || parsed.Path != "/api/monitor/usage/quota/limit" {
		return fmt.Errorf("invalid quota endpoint")
	}
	return nil
}

func parseQuotaResponse(raw []byte, observedAt time.Time) (quotaSnapshot, error) {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	var response quotaWireResponse
	if err := decoder.Decode(&response); err != nil {
		return quotaSnapshot{}, fmt.Errorf("malformed JSON")
	}
	if err := ensureJSONEOF(decoder); err != nil {
		return quotaSnapshot{}, fmt.Errorf("malformed JSON")
	}
	if response.Success != nil && !*response.Success {
		return quotaSnapshot{}, fmt.Errorf("upstream marked response unsuccessful")
	}
	if response.Code != "" {
		code, err := response.Code.Int64()
		if err != nil || code != http.StatusOK {
			return quotaSnapshot{}, fmt.Errorf("invalid response code")
		}
	}

	var fiveHour, weekly *quotaWindow
	for i := range response.Data.Limits {
		limit := response.Data.Limits[i]
		if strings.ToUpper(strings.TrimSpace(limit.Type)) != "CREDIT_LIMIT" {
			continue
		}
		unit, errUnit := strictInt64(limit.Unit)
		number, errNumber := strictInt64(limit.Number)
		if errUnit != nil || errNumber != nil {
			return quotaSnapshot{}, fmt.Errorf("limit has invalid unit or number")
		}
		var target **quotaWindow
		switch {
		case unit == 3 && number == 5:
			target = &fiveHour
		case unit == 6 && number == 1:
			target = &weekly
		default:
			continue
		}
		if *target != nil {
			return quotaSnapshot{}, fmt.Errorf("duplicate quota limit")
		}
		window, err := parseQuotaWindow(limit, observedAt)
		if err != nil {
			return quotaSnapshot{}, err
		}
		*target = &window
	}
	if fiveHour == nil || weekly == nil {
		return quotaSnapshot{}, fmt.Errorf("missing required quota limit")
	}
	plan := normalizeUpstreamPlan(response.Data.Level)
	if plan == "" {
		plan = normalizeUpstreamPlan(response.Data.PlanName)
	}
	if plan == "" {
		return quotaSnapshot{}, fmt.Errorf("invalid plan level")
	}
	return quotaSnapshot{Plan: plan, FiveHour: *fiveHour, Weekly: *weekly, ObservedAt: observedAt.UTC()}, nil
}

func parseQuotaWindow(limit quotaWireLimit, observedAt time.Time) (quotaWindow, error) {
	usage, err := strictNonnegativeInt64(limit.Usage)
	if err != nil || usage <= 0 {
		return quotaWindow{}, fmt.Errorf("quota limit has invalid usage")
	}
	current, err := strictNonnegativeInt64(limit.CurrentValue)
	if err != nil || current > usage {
		return quotaWindow{}, fmt.Errorf("quota limit has invalid currentValue")
	}
	if limit.Remaining != "" {
		if _, err = strictNonnegativeInt64(limit.Remaining); err != nil {
			return quotaWindow{}, fmt.Errorf("quota limit has invalid remaining")
		}
	}
	resetMilliseconds, err := strictNonnegativeInt64(limit.NextResetTime)
	if err != nil || resetMilliseconds <= 0 {
		return quotaWindow{}, fmt.Errorf("quota limit has invalid nextResetTime")
	}
	reset := time.UnixMilli(resetMilliseconds).UTC()
	if reset.Year() < 2000 || reset.Year() > 2200 || !reset.After(observedAt.UTC()) {
		return quotaWindow{}, fmt.Errorf("quota limit has invalid nextResetTime")
	}
	if usage > int64(^uint64(0)>>1)/creditScale || current > int64(^uint64(0)>>1)/creditScale {
		return quotaWindow{}, fmt.Errorf("quota credits exceed supported range")
	}
	return quotaWindow{ConsumedMicrocredits: current * creditScale, BucketMicrocredits: usage * creditScale, ResetsAt: reset}, nil
}

func strictInt64(number json.Number) (int64, error) {
	if number == "" {
		return 0, fmt.Errorf("missing number")
	}
	return number.Int64()
}

func strictNonnegativeInt64(number json.Number) (int64, error) {
	value, err := strictInt64(number)
	if err != nil || value < 0 {
		return 0, fmt.Errorf("invalid number")
	}
	return value, nil
}

func normalizeUpstreamPlan(raw string) string {
	value := strings.ToLower(strings.TrimSpace(raw))
	value = strings.TrimSpace(strings.TrimSuffix(value, "plan"))
	switch {
	case strings.Contains(value, "lite"):
		return "lite"
	case strings.Contains(value, "pro"):
		return "pro"
	case strings.Contains(value, "max"):
		return "max"
	default:
		return ""
	}
}
