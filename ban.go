package main

import _ "github.com/gosuda/ssh-chat/types"

func NewBanManager() *BanManager {
	return &BanManager{banned: make(map[string]struct{})}
}

func (b *BanManager) IsBanned(ip string) bool {
	b.mu.RLock()
	_, ok := b.banned[ip]
	b.mu.RUnlock()
	return ok
}

func (b *BanManager) Ban(ip string) {
	b.mu.Lock()
	b.banned[ip] = struct{}{}
	b.mu.Unlock()
}

var banManager = NewBanManager()

