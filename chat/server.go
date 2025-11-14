package chat

import (
	"fmt"
	"log"
	"math/rand"
	"strings"
	"sync"
	"time"
)

	type ChatServer struct {
	mu        sync.RWMutex
	Store     MessageStore // 메시지 저장소 인터페이스
	clients   map[*Client]struct{}
	ipCounts  map[string]int  // IP당 연결 수 추적
	nicknames map[string]bool // 사용 중인 닉네임 추적
}
func NewChatServer(dbPath string) (*ChatServer, error) {
	store, err := NewSQLiteMessageStore(dbPath)
	if err != nil {
		return nil, fmt.Errorf("SQLite 메시지 저장소 생성 실패: %w", err)
	}

	if err := store.Init(); err != nil {
		return nil, fmt.Errorf("메시지 저장소 초기화 실패: %w", err)
	}

	cs := &ChatServer{
		Store:     store,
		clients:   make(map[*Client]struct{}),
		ipCounts:  make(map[string]int),
		nicknames: make(map[string]bool),
	}
	welcome := Message{
		Time:  getTimeInSeoul(),
		Nick:  "server",
		Text:  "Welcome to the SSH chat! Use ↑/↓ to scroll and Enter to send messages.",
		Color: 37,
	}
	// 환영 메시지를 데이터베이스에 저장
	if err := cs.Store.AppendMessage(welcome); err != nil {
		log.Printf("환영 메시지 저장 실패: %v", err)
	}
	cs.logMessage(welcome)
	return cs, nil
}

func (cs *ChatServer) AddClient(c *Client) {
	cs.mu.Lock()
	cs.clients[c] = struct{}{}
	cs.ipCounts[c.ip]++
	cs.nicknames[c.nickname] = true
	cs.mu.Unlock()
}

func (cs *ChatServer) RemoveClient(c *Client) {
	cs.mu.Lock()
	delete(cs.clients, c)
	cs.ipCounts[c.ip]--
	if cs.ipCounts[c.ip] <= 0 {
		delete(cs.ipCounts, c.ip)
	}
	delete(cs.nicknames, c.nickname)
	cs.mu.Unlock()
}

func (cs *ChatServer) AppendMessage(msg Message) {
	// 메시지에서 멘션 감지
	msg.Mentions = extractMentions(msg.Text)

	// 메시지를 데이터베이스에 저장
	if err := cs.Store.AppendMessage(msg); err != nil {
		log.Printf("메시지 데이터베이스 저장 실패: %v", err)
	}

	cs.mu.Lock()
	clients := make([]*Client, 0, len(cs.clients))
	for c := range cs.clients {
		clients = append(clients, c)
	}
	cs.mu.Unlock()

	cs.logMessage(msg)

	// 모든 클라이언트에게 알림 전송, 멘션된 사용자에게는 벨 소리 포함
	for _, client := range clients {
		isMentioned := false
		for _, mention := range msg.Mentions {
			if strings.EqualFold(client.nickname, mention) {
				isMentioned = true
				break
			}
		}
		client.NotifyWithBell(isMentioned)
	}
}

func (cs *ChatServer) AppendSystemMessage(text string) {
	cs.AppendMessage(Message{
		Time:  getTimeInSeoul(),
		Nick:  "server",
		Text:  text,
		Color: 37,
	})
}

// DisconnectByIP closes all clients currently connected from the given IP.
func (cs *ChatServer) DisconnectByIP(ip string) int {
	cs.mu.RLock()
	clients := make([]*Client, 0, len(cs.clients))
	for c := range cs.clients {
		if c.ip == ip {
			clients = append(clients, c)
		}
	}
	cs.mu.RUnlock()
	for _, c := range clients {
		// Best-effort notify and close
		_ = c.session.Exit(1)
		c.Close()
	}
	return len(clients)
}

func (cs *ChatServer) Messages() []Message {
	// 최신 1000개의 메시지를 데이터베이스에서 조회
	messages, err := cs.Store.GetMessages(0, 1000)
	if err != nil {
		log.Printf("메시지 조회 실패: %v", err)
		return nil
	}
	return messages
}

// Close는 ChatServer의 리소스를 정리합니다 (예: 데이터베이스 연결 닫기).
func (cs *ChatServer) Close() error {
	log.Println("ChatServer 리소스 정리 중...")
	return cs.Store.Close()
}

func (cs *ChatServer) ClientCount() int {
	cs.mu.RLock()
	defer cs.mu.RUnlock()
	return len(cs.clients)
}

// CheckIPLimit returns true if the IP has not exceeded the connection limit (2)
func (cs *ChatServer) CheckIPLimit(ip string) bool {
	cs.mu.RLock()
	defer cs.mu.RUnlock()
	return cs.ipCounts[ip] < 2
}

// GetUniqueNickname returns a unique nickname by adding 8-character HEX suffix if needed
func (cs *ChatServer) GetUniqueNickname(baseNickname string) string {
	cs.mu.Lock()
	defer cs.mu.Unlock()

	// If nickname is not taken, return as-is
	if !cs.nicknames[baseNickname] {
		return baseNickname
	}

	// Generate unique nickname with 8-character HEX suffix
	for i := 0; i < 100; i++ { // Try up to 100 times to avoid infinite loop
		suffix := fmt.Sprintf("%08x", rand.Int31())
		uniqueNickname := fmt.Sprintf("%s-%s", baseNickname, suffix)
		if !cs.nicknames[uniqueNickname] {
			return uniqueNickname
		}
	}

	// Fallback: use timestamp as suffix
	suffix := fmt.Sprintf("%08x", time.Now().Unix())
	return fmt.Sprintf("%s-%s", baseNickname, suffix)
}

func (cs *ChatServer) logMessage(msg Message) {
	sanitized := strings.ReplaceAll(msg.Text, "\n", "\\n")
	if len(sanitized) > 20 {
		sanitized = sanitized[:20]
	}
	if msg.IP != "" {
		log.Printf("%s [%s@%s] %s", msg.Time.Format(time.RFC3339), msg.Nick, msg.IP, sanitized)
		return
	}
	log.Printf("%s [%s] %s", msg.Time.Format(time.RFC3339), msg.Nick, sanitized)
}

// getTimeInSeoul returns the current time in the Asia/Seoul timezone.
// It falls back to UTC if the timezone cannot be loaded.
func getTimeInSeoul() time.Time {
	loc, err := time.LoadLocation("Asia/Seoul")
	if err != nil {
		log.Printf("Error loading Asia/Seoul timezone: %v. Using UTC.", err)
		return time.Now().UTC()
	}
	return time.Now().In(loc)
}