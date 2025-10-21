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
	messages  []Message
	clients   map[*Client]struct{}
	ipCounts  map[string]int  // Track connections per IP
	nicknames map[string]bool // Track used nicknames
}

func NewChatServer() *ChatServer {
	cs := &ChatServer{
		clients:   make(map[*Client]struct{}),
		ipCounts:  make(map[string]int),
		nicknames: make(map[string]bool),
	}
	welcome := Message{
		Time:  time.Now(),
		Nick:  "server",
		Text:  "Welcome to the SSH chat! Use ↑/↓ to scroll and Enter to send messages.",
		Color: 37,
	}
	cs.messages = append(cs.messages, welcome)
	cs.logMessage(welcome)
	return cs
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
	// Detect mentions in the message
	msg.Mentions = extractMentions(msg.Text)

	cs.mu.Lock()
	cs.messages = append(cs.messages, msg)
	clients := make([]*Client, 0, len(cs.clients))
	for c := range cs.clients {
		clients = append(clients, c)
	}
	cs.mu.Unlock()

	cs.logMessage(msg)

	// Send notifications to all clients, with bell for mentioned users
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
		Time:  time.Now(),
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
	cs.mu.RLock()
	defer cs.mu.RUnlock()
	out := make([]Message, len(cs.messages))
	copy(out, cs.messages)
	return out
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
