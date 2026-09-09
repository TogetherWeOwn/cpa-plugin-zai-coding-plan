package main

import (
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

const (
	maxFailureBodyBytes = 16 << 10
	maxResetHintLength  = 256
	maxResetHintFuture  = 8 * 24 * time.Hour
)

var resetHeaderNames = []string{
	"Retry-After",
	"X-RateLimit-Reset",
	"X-Rate-Limit-Reset",
	"RateLimit-Reset",
}

var resetKeyReplacer = strings.NewReplacer("_", "", "-", "", ".", "", " ", "")

var resetBodyKeys = map[string]struct{}{
	"retryafter":     {},
	"retryafterms":   {},
	"reset":          {},
	"resetat":        {},
	"resettime":      {},
	"nextresettime":  {},
	"ratelimitreset": {},
}

func rateLimitReset(now time.Time, state accountHealthState, fallback time.Duration) (time.Time, string) {
	if state.CapacityResetAt.After(now) {
		return state.CapacityResetAt, "authoritative quota reset"
	}
	return now.Add(fallback), "conservative rate-limit cooldown"
}

func parseRateLimitHint(record pluginapi.UsageRecord, now time.Time) (time.Time, string, bool) {
	for _, name := range resetHeaderNames {
		for _, value := range record.ResponseHeaders.Values(name) {
			if name == "Retry-After" {
				if resetAt, ok := parseRetryAfter(value, now); ok {
					return resetAt, "retry-after header", true
				}
				continue
			}
			if resetAt, ok := parseResetValue(value, name, now); ok {
				return resetAt, strings.ToLower(name) + " header", true
			}
		}
	}
	body := record.Failure.Body
	if len(body) == 0 || len(body) > maxFailureBodyBytes {
		return time.Time{}, "", false
	}
	decoder := json.NewDecoder(strings.NewReader(body))
	decoder.UseNumber()
	var value any
	if err := decoder.Decode(&value); err != nil || ensureJSONEOF(decoder) != nil {
		return time.Time{}, "", false
	}
	return findResetValue(value, now, 0)
}

func findResetValue(value any, now time.Time, depth int) (time.Time, string, bool) {
	if depth > 6 {
		return time.Time{}, "", false
	}
	switch typed := value.(type) {
	case map[string]any:
		for key, item := range typed {
			normalized := normalizeResetKey(key)
			if _, allowed := resetBodyKeys[normalized]; allowed {
				if resetAt, ok := parseBodyResetValue(item, normalized, now); ok {
					return resetAt, "rate-limit response body", true
				}
			}
			if resetAt, source, ok := findResetValue(item, now, depth+1); ok {
				return resetAt, source, true
			}
		}
	case []any:
		for _, item := range typed {
			if resetAt, source, ok := findResetValue(item, now, depth+1); ok {
				return resetAt, source, true
			}
		}
	}
	return time.Time{}, "", false
}

func parseBodyResetValue(value any, key string, now time.Time) (time.Time, bool) {
	switch typed := value.(type) {
	case json.Number:
		return parseResetValue(typed.String(), key, now)
	case float64:
		return parseResetValue(strconv.FormatFloat(typed, 'f', -1, 64), key, now)
	case string:
		return parseResetValue(typed, key, now)
	default:
		return time.Time{}, false
	}
}

func parseRetryAfter(raw string, now time.Time) (time.Time, bool) {
	value := strings.TrimSpace(raw)
	if value == "" || len(value) > maxResetHintLength {
		return time.Time{}, false
	}
	if seconds, err := strconv.ParseInt(value, 10, 64); err == nil {
		if seconds <= 0 || seconds > int64(maxResetHintFuture/time.Second) {
			return time.Time{}, false
		}
		return validResetAt(now.Add(time.Duration(seconds)*time.Second), now)
	}
	if parsed, err := http.ParseTime(value); err == nil {
		return validResetAt(parsed, now)
	}
	return time.Time{}, false
}

func parseResetValue(raw, key string, now time.Time) (time.Time, bool) {
	value := strings.TrimSpace(raw)
	if value == "" || len(value) > maxResetHintLength {
		return time.Time{}, false
	}
	if parsed, err := time.Parse(time.RFC3339Nano, value); err == nil {
		return validResetAt(parsed, now)
	}
	integer, err := strconv.ParseInt(value, 10, 64)
	if err != nil || integer <= 0 {
		return time.Time{}, false
	}
	if key == "retryafter" {
		return parseRetryAfter(value, now)
	}
	var resetAt time.Time
	switch {
	case strings.Contains(key, "ms") || integer >= 1_000_000_000_000:
		resetAt = time.UnixMilli(integer)
	case integer >= 1_000_000_000:
		resetAt = time.Unix(integer, 0)
	case integer <= int64(maxResetHintFuture/time.Second):
		resetAt = now.Add(time.Duration(integer) * time.Second)
	default:
		return time.Time{}, false
	}
	return validResetAt(resetAt, now)
}

func validResetAt(resetAt, now time.Time) (time.Time, bool) {
	resetAt = resetAt.UTC()
	now = now.UTC()
	if !resetAt.After(now) || resetAt.After(now.Add(maxResetHintFuture)) {
		return time.Time{}, false
	}
	return resetAt, true
}

func normalizeResetKey(key string) string {
	return resetKeyReplacer.Replace(strings.ToLower(strings.TrimSpace(key)))
}
