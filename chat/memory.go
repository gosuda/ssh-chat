package chat

import (
	"sync"
	"github.com/iwanhae/ssh-chat/types"
)

// MemoryMessageStore is a simple in-memory message store.
type MemoryMessageStore struct {
	mu       sync.RWMutex
	messages []Message
	users    map[string]int
	bans     map[string]*types.Ban
}

// NewMemoryMessageStore creates a new MemoryMessageStore.
func NewMemoryMessageStore() *MemoryMessageStore {
	return &MemoryMessageStore{
		messages: make([]Message, 0),
		users:    make(map[string]int),
		bans:     make(map[string]*types.Ban),
	}
}

func (s *MemoryMessageStore) Init() error {
	return nil
}

// AppendMessage appends a message to the store.
func (s *MemoryMessageStore) AppendMessage(msg Message) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.messages = append(s.messages, msg)
	if len(s.messages) > 4000 {
		s.messages = s.messages[len(s.messages)-4000:]
	}
	return nil
}

// GetMessages returns messages from the store.
func (s *MemoryMessageStore) GetMessages(offset, limit int) ([]Message, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	end := len(s.messages) - offset
	if end < 0 {
		end = 0
	}
	start := end - limit
	if start < 0 {
		start = 0
	}
	return s.messages[start:end], nil
}

func (s *MemoryMessageStore) GetMessageCount() (int, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return len(s.messages), nil
}

func (s *MemoryMessageStore) Close() error {
	return nil
}

func (s *MemoryMessageStore) GetUserColor(nick string) (int, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.users[nick], nil
}

func (s *MemoryMessageStore) SetUserColor(nick string, color int) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.users[nick] = color
	return nil
}

func (s *MemoryMessageStore) CreateUser(nick string, color int) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.users[nick] = color
	return nil
}

func (s *MemoryMessageStore) GetBans() ([]*types.Ban, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	bans := make([]*types.Ban, 0, len(s.bans))
	for _, ban := range s.bans {
		bans = append(bans, ban)
	}
	return bans, nil
}

func (s *MemoryMessageStore) SaveBan(ban *types.Ban) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.bans[ban.IP] = ban
	return nil
}

func (s *MemoryMessageStore) RemoveBan(ban *types.Ban) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.bans, ban.IP)
	return nil
}
