package main

import (
	"fmt"
	"hash/fnv"
	"strings"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

var sessionHeaderNames = []string{
	"X-Session-ID",
	"Session-Id",
	"Session_id",
	"X-Client-Request-Id",
}

type routingState struct {
	cursors map[string]uint64
}

func (state *routingState) initialize() {
	if state.cursors == nil {
		state.cursors = make(map[string]uint64)
	}
}

func (state *routingState) next(scope string, candidates []pluginapi.SchedulerAuthCandidate) string {
	const maxRoutingScopes = 1024
	if _, exists := state.cursors[scope]; !exists && len(state.cursors) >= maxRoutingScopes {
		clear(state.cursors)
	}
	cursor := state.cursors[scope]
	selected := candidates[int(cursor%uint64(len(candidates)))].ID
	state.cursors[scope] = cursor + 1
	return selected
}

func validateSchedulerDeployment(plugins cpaPluginsProjection) error {
	if !plugins.Enabled {
		return fmt.Errorf("plugins must be enabled")
	}
	item, exists := plugins.Configs[pluginID]
	if !exists || item.Enabled == nil || !*item.Enabled {
		return fmt.Errorf("%s must be enabled", pluginID)
	}
	if item.Priority != requiredPluginPriority {
		return fmt.Errorf("%s priority must be %d", pluginID, requiredPluginPriority)
	}
	for id, configured := range plugins.Configs {
		if id == pluginID || configured.Enabled == nil || !*configured.Enabled {
			continue
		}
		return fmt.Errorf("%s must be the sole enabled scheduler plugin; disable %s", pluginID, id)
	}
	return nil
}

func (r *pluginRuntime) pick(req pluginapi.SchedulerPickRequest) (pluginapi.SchedulerPickResponse, error) {
	now := r.runtimeNow()
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.stopped || r.snapshot == nil {
		return pluginapi.SchedulerPickResponse{}, newSchedulerError("zai_not_configured", "Z.ai scheduler is not configured")
	}

	managedCount := 0
	impairedCount := 0
	for _, candidate := range req.Candidates {
		identity := r.snapshot.byAuthID[strings.TrimSpace(candidate.ID)]
		if identity == "" {
			if recognizedZAICandidate(candidate) {
				return pluginapi.SchedulerPickResponse{}, newSchedulerError("zai_unmanaged_candidate", "recognized Z.ai credential is absent from the validated account snapshot")
			}
			continue
		}
		managedCount++
		if r.snapshot.Health[identity].assess(r.snapshot.byIdentity[identity], now).Status != healthHealthy {
			impairedCount++
		}
	}
	if managedCount == 0 {
		return pluginapi.SchedulerPickResponse{Handled: false}, nil
	}
	if impairedCount == 0 {
		return pluginapi.SchedulerPickResponse{Handled: true, DelegateBuiltin: pluginapi.SchedulerBuiltinRoundRobin}, nil
	}

	healthy := make([]pluginapi.SchedulerAuthCandidate, 0, managedCount-impairedCount)
	for _, candidate := range req.Candidates {
		identity := r.snapshot.byAuthID[strings.TrimSpace(candidate.ID)]
		if identity != "" && r.snapshot.Health[identity].assess(r.snapshot.byIdentity[identity], now).Status == healthHealthy {
			healthy = append(healthy, candidate)
		}
	}
	if len(healthy) == 0 {
		return pluginapi.SchedulerPickResponse{}, newSchedulerError("zai_no_capacity", "no healthy managed Z.ai capacity remains")
	}

	scope := schedulerScope(req)
	if sessionID := schedulerSessionID(req.Options.Headers); sessionID != "" {
		return pluginapi.SchedulerPickResponse{Handled: true, AuthID: healthy[stableIndex(sessionID, len(healthy))].ID}, nil
	}
	return pluginapi.SchedulerPickResponse{Handled: true, AuthID: r.snapshot.Routing.next(scope, healthy)}, nil
}

func recognizedZAICandidate(candidate pluginapi.SchedulerAuthCandidate) bool {
	baseURL := candidate.Attributes["base_url"]
	if _, recognized := recognizedBaseURL(baseURL, zaiAnthropicBaseURL); recognized {
		return true
	}
	if _, recognized := recognizedBaseURL(baseURL, zaiOpenAIBaseURL); recognized {
		return true
	}
	return strings.EqualFold(strings.TrimSpace(candidate.Attributes["compat_name"]), zaiCompatName) ||
		strings.EqualFold(strings.TrimSpace(candidate.Attributes["provider_key"]), zaiCompatName)
}

func schedulerScope(req pluginapi.SchedulerPickRequest) string {
	providers := append([]string{strings.ToLower(strings.TrimSpace(req.Provider))}, req.Providers...)
	for i := range providers {
		providers[i] = strings.ToLower(strings.TrimSpace(providers[i]))
	}
	return strings.Join(providers, ",") + "\x00" + strings.ToLower(strings.TrimSpace(req.Model))
}

func schedulerSessionID(headers map[string][]string) string {
	for _, name := range sessionHeaderNames {
		for key, values := range headers {
			if !strings.EqualFold(strings.TrimSpace(key), name) {
				continue
			}
			for _, value := range values {
				if value = strings.TrimSpace(value); value != "" {
					return value
				}
			}
		}
	}
	return ""
}

func stableIndex(value string, length int) int {
	if length <= 1 {
		return 0
	}
	hash := fnv.New64a()
	_, _ = hash.Write([]byte(value))
	return int(hash.Sum64() % uint64(length))
}
