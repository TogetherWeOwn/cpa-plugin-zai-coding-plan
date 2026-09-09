package main

func newRuntimeSnapshot(config pluginConfig, accounts []account, store *secureStore) *runtimeSnapshot {
	health := make(map[string]accountHealthState, len(accounts))
	byAuthID := make(map[string]string, len(accounts)*2)
	byIdentity := make(map[string]account, len(accounts))
	for i := range accounts {
		health[accounts[i].Identity] = accountHealthState{}
		byAuthID[accounts[i].ClaudeAuthID] = accounts[i].Identity
		byAuthID[accounts[i].OpenAIAuthID] = accounts[i].Identity
		byIdentity[accounts[i].Identity] = accounts[i]
	}
	return &runtimeSnapshot{
		Config:     config,
		Accounts:   accounts,
		Store:      store,
		Health:     health,
		byAuthID:   byAuthID,
		byIdentity: byIdentity,
	}
}
