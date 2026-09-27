package opencodego

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"
)

const usageFixtureKey = "test-only-dashboard-key"

var usageObservedAt = time.Date(2026, time.September, 13, 5, 0, 0, 0, time.UTC)

func TestParseUsageResponseMapsRollingWeeklyMonthly(t *testing.T) {
	raw := usageFixture(
		usageWindowFixture("ok", 42, usageObservedAt.Add(time.Hour)),
		usageWindowFixture("ok", 10, usageObservedAt.Add(7*24*time.Hour)),
		usageWindowFixture("ok", 5, usageObservedAt.Add(30*24*time.Hour)),
	)
	snapshot, err := parseUsageResponse([]byte(raw), usageObservedAt)
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.FiveHour.Utilization != 0.42 || snapshot.Weekly.Utilization != 0.10 || snapshot.Monthly.Utilization != 0.05 {
		t.Fatalf("snapshot = %#v", snapshot)
	}
	if !snapshot.FiveHour.ResetAt.Equal(usageObservedAt.Add(time.Hour)) || !snapshot.Weekly.ResetAt.Equal(usageObservedAt.Add(7*24*time.Hour)) || !snapshot.Monthly.ResetAt.Equal(usageObservedAt.Add(30*24*time.Hour)) {
		t.Fatalf("reset conversion failed: %#v", snapshot)
	}
	if snapshot.FiveHour.LimitReached || snapshot.Weekly.LimitReached || snapshot.Monthly.LimitReached {
		t.Fatalf("non-exhausted windows marked limit reached: %#v", snapshot)
	}
}

func TestParseUsageResponseRateLimitedForcesFullUtilization(t *testing.T) {
	raw := usageFixture(
		usageWindowFixture("rate-limited", 0, usageObservedAt.Add(time.Hour)),
		usageWindowFixture("ok", 1, usageObservedAt.Add(7*24*time.Hour)),
		usageWindowFixture("ok", 1, usageObservedAt.Add(30*24*time.Hour)),
	)
	snapshot, err := parseUsageResponse([]byte(raw), usageObservedAt)
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.FiveHour.Utilization != 1 || !snapshot.FiveHour.LimitReached {
		t.Fatalf("rate-limited window = %#v, want forced 100%%", snapshot.FiveHour)
	}
}

func TestParseUsageResponsePercentHundredSetsLimitReached(t *testing.T) {
	raw := usageFixture(
		usageWindowFixture("ok", 100, usageObservedAt.Add(time.Hour)),
		usageWindowFixture("ok", 1, usageObservedAt.Add(7*24*time.Hour)),
		usageWindowFixture("ok", 1, usageObservedAt.Add(30*24*time.Hour)),
	)
	snapshot, err := parseUsageResponse([]byte(raw), usageObservedAt)
	if err != nil {
		t.Fatal(err)
	}
	if !snapshot.FiveHour.LimitReached || snapshot.FiveHour.Utilization != 1 {
		t.Fatalf("100%% window = %#v", snapshot.FiveHour)
	}
}

func TestParseUsageResponseRejectsMalformedIncompleteAndOutOfRange(t *testing.T) {
	validFive := usageWindowFixture("ok", 42, usageObservedAt.Add(time.Hour))
	validWeekly := usageWindowFixture("ok", 10, usageObservedAt.Add(7*24*time.Hour))
	validMonthly := usageWindowFixture("ok", 5, usageObservedAt.Add(30*24*time.Hour))
	tests := []struct {
		name string
		raw  string
	}{
		{name: "malformed", raw: `{`},
		{name: "trailing bytes", raw: usageFixture(validFive, validWeekly, validMonthly) + `garbage`},
		{name: "unrecognized status", raw: usageFixture(usageWindowFixture("unknown", 1, usageObservedAt.Add(time.Hour)), validWeekly, validMonthly)},
		{name: "missing percent", raw: strings.Replace(usageFixture(validFive, validWeekly, validMonthly), `,"percent":42`, ``, 1)},
		{name: "percent negative", raw: strings.Replace(usageFixture(validFive, validWeekly, validMonthly), `"percent":42`, `"percent":-1`, 1)},
		{name: "percent over 100", raw: strings.Replace(usageFixture(validFive, validWeekly, validMonthly), `"percent":42`, `"percent":101`, 1)},
		{name: "missing resetsAt", raw: strings.Replace(usageFixture(validFive, validWeekly, validMonthly), usageObservedAt.Add(time.Hour).Format(time.RFC3339), "", 1)},
		{name: "unparseable resetsAt", raw: strings.Replace(usageFixture(validFive, validWeekly, validMonthly), usageObservedAt.Add(time.Hour).Format(time.RFC3339), "not-a-time", 1)},
		{name: "resetsAt too far future", raw: strings.Replace(usageFixture(validFive, validWeekly, validMonthly), usageObservedAt.Add(time.Hour).Format(time.RFC3339), usageObservedAt.Add(maxResetFuture+time.Hour).Format(time.RFC3339), 1)},
		{name: "resetsAt not after observedAt for non-exhausted", raw: strings.Replace(usageFixture(validFive, validWeekly, validMonthly), usageObservedAt.Add(time.Hour).Format(time.RFC3339), usageObservedAt.Format(time.RFC3339), 1)},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := parseUsageResponse([]byte(tt.raw), usageObservedAt); err == nil {
				t.Fatal("invalid usage payload succeeded")
			}
		})
	}
}

func TestFetchUsageUsesBearerWithoutLeakingKey(t *testing.T) {
	client := roundTripDoer(func(request *http.Request) (*http.Response, error) {
		if got := request.Header.Get("Authorization"); got != "Bearer "+usageFixtureKey {
			t.Fatalf("authorization = %q", got)
		}
		return usageHTTPResponse(http.StatusOK, usageFixture(
			usageWindowFixture("ok", 42, usageObservedAt.Add(time.Hour)),
			usageWindowFixture("ok", 10, usageObservedAt.Add(7*24*time.Hour)),
			usageWindowFixture("ok", 5, usageObservedAt.Add(30*24*time.Hour)),
		)), nil
	})
	snapshot, err := fetchUsage(context.Background(), client, usageEndpoint, usageFixtureKey, usageObservedAt)
	if err != nil || snapshot.FiveHour.Utilization != 0.42 {
		t.Fatalf("snapshot = %#v, err = %v", snapshot, err)
	}
	serialized, marshalErr := json.Marshal(snapshot)
	if marshalErr != nil {
		t.Fatal(marshalErr)
	}
	if strings.Contains(string(serialized), usageFixtureKey) || strings.Contains(errString(err), usageFixtureKey) {
		t.Fatalf("usage result leaked key: %s %v", serialized, err)
	}
}

func TestFetchUsageErrorsAreRedactedAndBounded(t *testing.T) {
	client := roundTripDoer(func(*http.Request) (*http.Response, error) {
		return nil, errors.New("network failed with " + usageFixtureKey)
	})
	_, err := fetchUsage(context.Background(), client, usageEndpoint, usageFixtureKey, usageObservedAt)
	if err == nil || strings.Contains(err.Error(), usageFixtureKey) {
		t.Fatalf("error = %v", err)
	}
}

// TestFetchUsageRejectsUnauthorizedWithoutEchoingBody proves the fetcher
// fails closed against the exact D01 fixture 401 body without ever
// including that body (or the key) in a returned error.
func TestFetchUsageRejectsUnauthorizedWithoutEchoingBody(t *testing.T) {
	const d01Body = `{"type":"error","error":{"type":"AuthError","message":"Missing API key."}}`
	client := roundTripDoer(func(*http.Request) (*http.Response, error) {
		return usageHTTPResponse(http.StatusUnauthorized, d01Body), nil
	})
	_, err := fetchUsage(context.Background(), client, usageEndpoint, usageFixtureKey, usageObservedAt)
	if err == nil {
		t.Fatal("expected error for 401 response")
	}
	if strings.Contains(err.Error(), "AuthError") || strings.Contains(err.Error(), "Missing API key") || strings.Contains(err.Error(), usageFixtureKey) {
		t.Fatalf("error leaked response body or key: %v", err)
	}
}

func TestFetchUsageRejectsOversizedResponse(t *testing.T) {
	client := roundTripDoer(func(*http.Request) (*http.Response, error) {
		return usageHTTPResponse(http.StatusOK, strings.Repeat("a", maxUsageResponse+1)), nil
	})
	if _, err := fetchUsage(context.Background(), client, usageEndpoint, usageFixtureKey, usageObservedAt); err == nil {
		t.Fatal("expected oversized response to be rejected")
	}
}

func TestFetchUsageHonorsContextTimeout(t *testing.T) {
	client := roundTripDoer(func(request *http.Request) (*http.Response, error) {
		<-request.Context().Done()
		return nil, request.Context().Err()
	})
	ctx, cancel := context.WithTimeout(context.Background(), time.Millisecond)
	defer cancel()
	if _, err := fetchUsage(ctx, client, usageEndpoint, usageFixtureKey, usageObservedAt); err == nil || strings.Contains(err.Error(), usageFixtureKey) {
		t.Fatalf("error = %v", err)
	}
}

func TestValidateUsageEndpointLocksToProductionHostInNonTestMode(t *testing.T) {
	tests := []struct {
		name    string
		invalid string
	}{
		{name: "wrong host", invalid: "https://evil.example/zen/go/v1/usage"},
		{name: "wrong path", invalid: "https://opencode.ai/other/path"},
		{name: "http scheme", invalid: "http://opencode.ai/zen/go/v1/usage"},
		{name: "with query", invalid: "https://opencode.ai/zen/go/v1/usage?x=1"},
		{name: "with userinfo", invalid: "https://user@opencode.ai/zen/go/v1/usage"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if err := validateUsageEndpoint(tt.invalid, false); err == nil {
				t.Fatalf("expected %q to be rejected in production mode", tt.invalid)
			}
		})
	}
	if err := validateUsageEndpoint(usageEndpoint, false); err != nil {
		t.Fatalf("production endpoint rejected: %v", err)
	}
}

func TestValidateUsageEndpointAllowsLocalhostOnlyForTests(t *testing.T) {
	if err := validateUsageEndpoint("http://127.0.0.1:9999/usage", true); err != nil {
		t.Fatalf("localhost endpoint rejected in test mode: %v", err)
	}
	if err := validateUsageEndpoint("http://127.0.0.1:9999/usage", false); err == nil {
		t.Fatal("localhost endpoint accepted outside test mode")
	}
}

func TestNewUsageHTTPClientRejectsRedirectsAndAmbientProxy(t *testing.T) {
	client := newUsageHTTPClient(0)
	if err := client.CheckRedirect(&http.Request{}, nil); !errors.Is(err, http.ErrUseLastResponse) {
		t.Fatalf("redirect error = %v", err)
	}
	transport, ok := client.Transport.(*http.Transport)
	if !ok || transport.Proxy != nil {
		t.Fatalf("transport = %#v, ambient proxy must be disabled", client.Transport)
	}
}

func TestNewUsageHTTPClientHonorsConfiguredTimeoutAboveDefault(t *testing.T) {
	client := newUsageHTTPClient(30 * time.Second)
	if client.Timeout != 30*time.Second {
		t.Fatalf("client timeout = %s, want 30s", client.Timeout)
	}
	if newUsageHTTPClient(0).Timeout != defaultUsageTimeout {
		t.Fatalf("zero timeout did not fall back to default")
	}
}

func usageFixture(rolling, weekly, monthly string) string {
	return `{"usage":{"rolling":` + rolling + `,"weekly":` + weekly + `,"monthly":` + monthly + `}}`
}

func usageWindowFixture(status string, percent int, resetsAt time.Time) string {
	return `{"status":"` + status + `","percent":` + itoa(percent) + `,"resetsAt":"` + resetsAt.Format(time.RFC3339) + `"}`
}

func itoa(value int) string {
	if value == 0 {
		return "0"
	}
	negative := value < 0
	if negative {
		value = -value
	}
	var digits []byte
	for value > 0 {
		digits = append([]byte{byte('0' + value%10)}, digits...)
		value /= 10
	}
	if negative {
		return "-" + string(digits)
	}
	return string(digits)
}

func usageHTTPResponse(status int, body string) *http.Response {
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
