package coordinator

import (
	"fmt"
	"sort"
	"sync"
)

// registryEntry records which module owns a dispatched auth ID.
type registryEntry struct {
	providerID string
}

// registry maps auth IDs to the provider module that owns them. It is
// rebuilt wholesale on every successful reconfigure and swapped in
// atomically, so readers never observe a partially rebuilt map.
type registry struct {
	mu     sync.RWMutex
	byAuth map[string]registryEntry
}

// buildRegistry collects every module's OwnedAuthIDs() and enforces
// invariant 1: no auth ID may be claimed by more than one provider. owned
// maps providerID to that provider's currently owned auth IDs.
func buildRegistry(owned map[string][]string) (map[string]registryEntry, error) {
	byAuth := make(map[string]registryEntry)
	providerIDs := make([]string, 0, len(owned))
	for providerID := range owned {
		providerIDs = append(providerIDs, providerID)
	}
	sort.Strings(providerIDs)
	for _, providerID := range providerIDs {
		for _, authID := range owned[providerID] {
			if authID == "" {
				continue
			}
			if existing, ok := byAuth[authID]; ok {
				return nil, fmt.Errorf("auth id %q is owned by both %q and %q", authID, existing.providerID, providerID)
			}
			byAuth[authID] = registryEntry{providerID: providerID}
		}
	}
	return byAuth, nil
}

// swap atomically replaces the registry's contents.
func (r *registry) swap(byAuth map[string]registryEntry) {
	r.mu.Lock()
	r.byAuth = byAuth
	r.mu.Unlock()
}

// providerFor returns the provider ID owning authID, or "" if unmapped.
func (r *registry) providerFor(authID string) string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.byAuth[authID].providerID
}
