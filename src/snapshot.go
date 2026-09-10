package main

func newRuntimeSnapshot(config pluginConfig, accounts []account, store *secureStore) *runtimeSnapshot {
	health := make(map[string]accountHealthState, len(accounts))
	byAuthID := make(map[string]string, len(accounts)*3)
	byIdentity := make(map[string]account, len(accounts))
	for i := range accounts {
		health[accounts[i].Identity] = accountHealthState{}
		indexAccountAuthIDs(byAuthID, accounts[i])
		byIdentity[accounts[i].Identity] = accounts[i]
	}
	return &runtimeSnapshot{
		Config:       config,
		BaseAccounts: append([]account(nil), accounts...),
		Accounts:     accounts,
		Store:        store,
		Health:       health,
		byAuthID:     byAuthID,
		byIdentity:   byIdentity,
	}
}

func accountAuthIDs(item account) []string {
	return uniqueStrings(append(append([]string(nil), item.claudeAuthIDs...), item.ClaudeAuthID, item.OpenAIAuthID)...)
}

func indexAccountAuthIDs(byAuthID map[string]string, item account) {
	for _, authID := range accountAuthIDs(item) {
		byAuthID[authID] = item.Identity
	}
}
