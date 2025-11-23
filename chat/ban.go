package chat

import (
	"fmt"
	"sync"
	"time"

	"github.com/iwanhae/ssh-chat/types"
)

// BanManager keeps a set of banned IP addresses.
type BanManager struct {
	mu     sync.RWMutex
	banned map[string]*types.Ban
	store  types.Store
}

// NewBanManager creates and returns a new BanManager.
func NewBanManager(store types.Store) *BanManager {
	return &BanManager{
		banned: make(map[string]*types.Ban),
		store:  store,
	}
}

func (b *BanManager) LoadBans(bans []*types.Ban) {
	b.mu.Lock()
	defer b.mu.Unlock()
	for _, ban := range bans {
		b.banned[ban.IP] = ban
	}
}

func (b *BanManager) GetBan(ip string) (*types.Ban, bool) {
	b.mu.RLock()
	defer b.mu.RUnlock()
	ban, ok := b.banned[ip]
	return ban, ok
}

func (b *BanManager) IsBanned(ip string) bool {
	_, ok := b.GetBan(ip)
	return ok
}

func (b *BanManager) Ban(ip string, BannedBy string, Reason string) error {
	ban := &types.Ban{
		IP:       ip,
		BannedBy: BannedBy,
		Reason:   Reason,
		At:       time.Now(),
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	b.banned[ip] = ban
	return b.store.SaveBan(ban)
}

func (b *BanManager) BanList() string {
	var banlist string
	b.mu.RLock()
	defer b.mu.RUnlock()
	for _, ban := range b.banned {
		banned := fmt.Sprintf("밴당한 IP: %s, 이유: %s, 밴한사람: %s\n", ban.IP, ban.Reason, ban.BannedBy)
		banlist += banned
	}
	return banlist
}

func (b *BanManager) Pardon(ip string) error {
	ban, ok := b.GetBan(ip)
	if !ok {
		return fmt.Errorf("ip not banned: %s", ip)
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	delete(b.banned, ip)
	return b.store.RemoveBan(ban)
}