package chat

import "sync"

// BanManager keeps a set of banned IP addresses.
type BanManager struct {
	mu     sync.RWMutex
	banned map[string]struct{}
}

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
