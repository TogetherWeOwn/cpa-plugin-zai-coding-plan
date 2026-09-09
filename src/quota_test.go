package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"
)

const quotaFixtureKey = "zai-plan-secret-never-serialize-4f9c31a7"

var quotaObservedAt = time.Date(2026, time.September, 8, 5, 0, 0, 0, time.UTC)

func TestParseQuotaResponseMapsProLimitsIndependentlyOfOrder(t *testing.T) {
	raw := quotaFixture("pro", []string{
		quotaLimitFixture(6, 1, 60_000, 10_800, 49_200, quotaObservedAt.Add(7*24*time.Hour).UnixMilli()),
		quotaLimitFixture(3, 5, 12_000, 5_040, 6_960, quotaObservedAt.Add(5*time.Hour).UnixMilli()),
	})
	snapshot, err := parseQuotaResponse([]byte(raw), quotaObservedAt)
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.Plan != "pro" || snapshot.FiveHour.ConsumedMicrocredits != 5_040*creditScale || snapshot.Weekly.BucketMicrocredits != 60_000*creditScale {
		t.Fatalf("snapshot = %#v", snapshot)
	}
	if !snapshot.FiveHour.ResetsAt.Equal(quotaObservedAt.Add(5*time.Hour)) || !snapshot.Weekly.ResetsAt.Equal(quotaObservedAt.Add(7*24*time.Hour)) {
		t.Fatalf("reset conversion failed: %#v", snapshot)
	}
}

func TestParseQuotaResponseAcceptsPlanName(t *testing.T) {
	raw := strings.Replace(quotaFixture("", []string{
		quotaLimitFixture(3, 5, 12_000, 1, 11_999, quotaObservedAt.Add(time.Hour).UnixMilli()),
		quotaLimitFixture(6, 1, 60_000, 2, 59_998, quotaObservedAt.Add(24*time.Hour).UnixMilli()),
	}), `"level":""`, `"level":"","planName":"Pro Plan"`, 1)
	snapshot, err := parseQuotaResponse([]byte(raw), quotaObservedAt)
	if err != nil || snapshot.Plan != "pro" {
		t.Fatalf("snapshot = %#v, err = %v", snapshot, err)
	}
}

func TestParseQuotaResponseRejectsIncompleteDuplicateAndInvalidNumbers(t *testing.T) {
	validFive := quotaLimitFixture(3, 5, 12_000, 1, 11_999, quotaObservedAt.Add(time.Hour).UnixMilli())
	validWeek := quotaLimitFixture(6, 1, 60_000, 2, 59_998, quotaObservedAt.Add(24*time.Hour).UnixMilli())
	tests := []struct {
		name string
		raw  string
	}{
		{name: "missing weekly", raw: quotaFixture("pro", []string{validFive})},
		{name: "duplicate five hour", raw: quotaFixture("pro", []string{validFive, validFive, validWeek})},
		{name: "fractional current value", raw: quotaFixture("pro", []string{strings.Replace(validFive, `"currentValue":1`, `"currentValue":1.5`, 1), validWeek})},
		{name: "negative usage", raw: quotaFixture("pro", []string{strings.Replace(validFive, `"usage":12000`, `"usage":-1`, 1), validWeek})},
		{name: "current exceeds usage", raw: quotaFixture("pro", []string{strings.Replace(validFive, `"currentValue":1`, `"currentValue":12001`, 1), validWeek})},
		{name: "invalid reset", raw: quotaFixture("pro", []string{validFive, strings.Replace(validWeek, stringNumber(quotaObservedAt.Add(24*time.Hour).UnixMilli()), "1", 1)})},
		{name: "unknown plan", raw: quotaFixture("enterprise", []string{validFive, validWeek})},
		{name: "malformed", raw: `{`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := parseQuotaResponse([]byte(tt.raw), quotaObservedAt); err == nil {
				t.Fatal("invalid quota payload succeeded")
			}
		})
	}
}

func TestFetchQuotaUsesBearerWithoutLeakingSecret(t *testing.T) {
	client := roundTripDoer(func(request *http.Request) (*http.Response, error) {
		if got := request.Header.Get("Authorization"); got != "Bearer "+quotaFixtureKey {
			t.Fatalf("authorization = %q", got)
		}
		return quotaHTTPResponse(http.StatusOK, quotaFixture("max", []string{
			quotaLimitFixture(3, 5, 28_000, 3_341, 24_659, quotaObservedAt.Add(time.Hour).UnixMilli()),
			quotaLimitFixture(6, 1, 140_000, 25_224, 114_776, quotaObservedAt.Add(24*time.Hour).UnixMilli()),
		})), nil
	})
	snapshot, err := fetchQuota(context.Background(), client, quotaEndpoint, quotaFixtureKey, quotaObservedAt)
	if err != nil || snapshot.Plan != "max" {
		t.Fatalf("snapshot = %#v, err = %v", snapshot, err)
	}
	serialized, err := json.Marshal(snapshot)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(serialized), quotaFixtureKey) || strings.Contains(errString(err), quotaFixtureKey) {
		t.Fatalf("quota result leaked key: %s %v", serialized, err)
	}
}

func TestFetchQuotaErrorsAreRedactedAndBounded(t *testing.T) {
	client := roundTripDoer(func(*http.Request) (*http.Response, error) {
		return nil, errors.New("network failed with " + quotaFixtureKey)
	})
	_, err := fetchQuota(context.Background(), client, quotaEndpoint, quotaFixtureKey, quotaObservedAt)
	if err == nil || strings.Contains(err.Error(), quotaFixtureKey) {
		t.Fatalf("error = %v", err)
	}
}

func TestFetchQuotaSelectsFallbackOnNon200MalformedAndTimeout(t *testing.T) {
	tests := []struct {
		name   string
		client httpDoer
	}{
		{name: "non-200", client: roundTripDoer(func(*http.Request) (*http.Response, error) { return quotaHTTPResponse(503, quotaFixtureKey), nil })},
		{name: "malformed", client: roundTripDoer(func(*http.Request) (*http.Response, error) { return quotaHTTPResponse(200, `{`), nil })},
		{name: "timeout", client: roundTripDoer(func(request *http.Request) (*http.Response, error) {
			<-request.Context().Done()
			return nil, request.Context().Err()
		})},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), time.Millisecond)
			defer cancel()
			if _, err := fetchQuota(ctx, tt.client, quotaEndpoint, quotaFixtureKey, quotaObservedAt); err == nil || strings.Contains(err.Error(), quotaFixtureKey) {
				t.Fatalf("error = %v", err)
			}
		})
	}
}

func TestQuotaHTTPClientRejectsRedirectsAndAmbientProxy(t *testing.T) {
	client := newQuotaHTTPClient()
	if err := client.CheckRedirect(&http.Request{}, nil); !errors.Is(err, http.ErrUseLastResponse) {
		t.Fatalf("redirect error = %v", err)
	}
	transport, ok := client.Transport.(*http.Transport)
	if !ok || transport.Proxy != nil {
		t.Fatalf("transport = %#v, ambient proxy must be disabled", client.Transport)
	}
}

func TestJitterAndBackoffAreDeterministicAndBounded(t *testing.T) {
	for _, base := range []time.Duration{time.Minute, 2 * time.Minute} {
		first := jitteredPollInterval(base, "account-a", 7)
		second := jitteredPollInterval(base, "account-a", 7)
		if first != second || first < base/2 || first > maxPollInterval(base) {
			t.Fatalf("jitter = %v/%v for base %v", first, second, base)
		}
	}
	if pollBackoff(time.Minute, 1) != time.Minute || pollBackoff(time.Minute, 6) != 15*time.Minute {
		t.Fatalf("unexpected backoff")
	}
}

func quotaFixture(level string, limits []string) string {
	return `{"code":200,"success":true,"data":{"limits":[` + strings.Join(limits, ",") + `],"level":"` + level + `"}}`
}

func quotaLimitFixture(unit, number, usage, current, remaining, reset int64) string {
	return `{"type":"CREDIT_LIMIT","unit":` + stringNumber(unit) + `,"number":` + stringNumber(number) + `,"usage":` + stringNumber(usage) + `,"currentValue":` + stringNumber(current) + `,"remaining":` + stringNumber(remaining) + `,"nextResetTime":` + stringNumber(reset) + `}`
}

func stringNumber(value int64) string { return fmt.Sprintf("%d", value) }

func quotaHTTPResponse(status int, body string) *http.Response {
	return &http.Response{StatusCode: status, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(body))}
}

type roundTripDoer func(*http.Request) (*http.Response, error)

func (function roundTripDoer) Do(request *http.Request) (*http.Response, error) {
	return function(request)
}

func errString(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}
