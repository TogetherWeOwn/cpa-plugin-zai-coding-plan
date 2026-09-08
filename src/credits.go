package main

import (
	"crypto/sha256"
	"encoding/binary"
	"fmt"
	"math/big"
	"strings"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

const creditScale int64 = 1_000_000

type creditRates struct {
	Input         int64
	Cached        int64
	Output        int64
	CanonicalName string
}

var modelCreditRates = map[string]creditRates{
	"glm-5.3":       {Input: 690, Cached: 170, Output: 2_400, CanonicalName: "glm-5.3"},
	"glm-5.3-flash": {Input: 230, Cached: 56, Output: 800, CanonicalName: "glm-5.3-flash"},
}

type creditEstimate struct {
	Microcredits int64
	Model        string
	Offpeak      bool
}

func estimateUsageCredits(record pluginapi.UsageRecord, at time.Time) (creditEstimate, error) {
	rates, ok := ratesForUsage(record)
	if !ok {
		return creditEstimate{}, fmt.Errorf("unpriced model")
	}
	detail := record.Detail
	if detail.InputTokens < 0 || detail.OutputTokens < 0 || detail.CacheReadTokens < 0 || detail.CacheCreationTokens < 0 {
		return creditEstimate{}, fmt.Errorf("negative token count")
	}

	// InputTokens, CacheReadTokens, and CacheCreationTokens are deliberately
	// treated as disjoint counters. Generic CachedTokens is never priced.
	total := new(big.Int)
	addScaledTokens(total, detail.InputTokens, rates.Input)
	addScaledTokens(total, detail.CacheCreationTokens, rates.Input)
	addScaledTokens(total, detail.CacheReadTokens, rates.Cached)
	addScaledTokens(total, detail.OutputTokens, rates.Output)

	offpeak := isOffpeak(at)
	if offpeak {
		total.Quo(total, big.NewInt(2))
	}
	if !total.IsInt64() {
		return creditEstimate{}, fmt.Errorf("credit estimate exceeds supported range")
	}
	return creditEstimate{Microcredits: total.Int64(), Model: rates.CanonicalName, Offpeak: offpeak}, nil
}

func addScaledTokens(total *big.Int, tokens, multiplier int64) {
	term := new(big.Int).Mul(big.NewInt(tokens), big.NewInt(multiplier))
	total.Add(total, term)
}

func ratesForUsage(record pluginapi.UsageRecord) (creditRates, bool) {
	for _, candidate := range []string{record.Model, record.Alias} {
		name := normalizeModelName(candidate)
		if rates, ok := modelCreditRates[name]; ok {
			return rates, true
		}
	}
	return creditRates{}, false
}

func normalizeModelName(raw string) string {
	name := strings.ToLower(strings.TrimSpace(raw))
	if slash := strings.LastIndexByte(name, '/'); slash >= 0 {
		name = name[slash+1:]
	}
	return name
}

func isOffpeak(at time.Time) bool {
	utc := at.UTC()
	weekday := utc.Weekday()
	if weekday == time.Saturday || weekday == time.Sunday {
		return true
	}
	hour := utc.Hour()
	return hour < 6 || hour >= 10
}

func usageDedupHash(record pluginapi.UsageRecord) string {
	hash := sha256.New()
	writeHashString(hash, record.AuthID)
	writeHashString(hash, record.Provider)
	writeHashString(hash, record.Model)
	writeHashString(hash, record.Alias)
	var values [11]int64
	values[0] = record.RequestedAt.UnixNano()
	values[1] = int64(record.Latency)
	values[2] = int64(record.Failure.StatusCode)
	values[3] = record.Detail.InputTokens
	values[4] = record.Detail.OutputTokens
	values[5] = record.Detail.ReasoningTokens
	values[6] = record.Detail.CacheReadTokens
	values[7] = record.Detail.CacheCreationTokens
	values[8] = record.Detail.TotalTokens
	values[9] = boolInt64(record.Failed)
	values[10] = int64(len(record.Failure.Body))
	var encoded [8]byte
	for _, value := range values {
		binary.LittleEndian.PutUint64(encoded[:], uint64(value))
		_, _ = hash.Write(encoded[:])
	}
	return fmt.Sprintf("%x", hash.Sum(nil))
}

func writeHashString(hash interface{ Write([]byte) (int, error) }, value string) {
	var length [8]byte
	binary.LittleEndian.PutUint64(length[:], uint64(len(value)))
	_, _ = hash.Write(length[:])
	_, _ = hash.Write([]byte(value))
}

func boolInt64(value bool) int64 {
	if value {
		return 1
	}
	return 0
}
