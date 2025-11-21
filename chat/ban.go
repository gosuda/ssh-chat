package chat

import (
	"fmt"
	"sync"
)

// BanManager keeps a set of banned IP addresses.
type BanManager struct {
	mu     sync.RWMutex
	banned map[string]struct{}
}

func (b *BanManager) IsBanned(ip string) bool {
	b.mu.RLock()
	defer b.mu.RUnlock()
	_, ok := b.banned[ip]
	return ok
}

func (b *BanManager) Ban(ip string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.banned[ip] = struct{}{}
}

func (b *BanManager) BanList() string {
	var banlist string
	b.mu.RLock()
	defer b.mu.RUnlock()
	for k, _ := range b.banned {
		banned := fmt.Sprintf("밴당한 IP: %s\n", k)
		banlist += banned
	}
	return banlist
}

func (b *BanManager) Pardon(ip string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	delete(b.banned, ip)
}
