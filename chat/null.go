package chat

import "github.com/iwanhae/ssh-chat/types"

// NullMessageStore is a message store that does nothing.
type NullMessageStore struct{}

// NewNullMessageStore creates a new NullMessageStore.
func NewNullMessageStore() *NullMessageStore {
	return &NullMessageStore{}
}

// Init does nothing.
func (s *NullMessageStore) Init() error {
	return nil
}

// AppendMessage does nothing.
func (s *NullMessageStore) AppendMessage(msg Message) error {
	return nil
}

// GetMessages returns no messages.
func (s *NullMessageStore) GetMessages(offset, limit int) ([]Message, error) {
	return []Message{}, nil
}

// GetMessageCount returns 0.
func (s *NullMessageStore) GetMessageCount() (int, error) {
	return 0, nil
}

// Close does nothing.
func (s *NullMessageStore) Close() error {
	return nil
}

// GetUserColor does nothing.
func (s *NullMessageStore) GetUserColor(nick string) (int, error) {
	return 0, nil
}

// SetUserColor does nothing.
func (s *NullMessageStore) SetUserColor(nick string, color int) error {
	return nil
}

// CreateUser does nothing.
func (s *NullMessageStore) CreateUser(nick string, color int) error {
	return nil
}

// GetBans returns no bans.
func (s *NullMessageStore) GetBans() ([]*types.Ban, error) {
	return []*types.Ban{}, nil
}

// SaveBan does nothing.
func (s *NullMessageStore) SaveBan(*types.Ban) error {
	return nil
}

// RemoveBan does nothing.
func (s *NullMessageStore) RemoveBan(*types.Ban) error {
	return nil
}
