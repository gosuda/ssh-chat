package main

import (
	"bufio"
	"errors"
	"fmt"
	"log"
	"net"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"
	"unicode"

	"github.com/gliderlabs/ssh"
	_ "./types"
)

func NewChatServer() *ChatServer {
	cs := &ChatServer{
		clients: make(map[*Client]struct{}),
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
	cs.mu.Unlock()
}

func (cs *ChatServer) RemoveClient(c *Client) {
	cs.mu.Lock()
	delete(cs.clients, c)
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


func main() {
	quitCh := make(chan os.Signal, 1)
	signal.Notify(quitCh, os.Interrupt, syscall.SIGTERM, syscall.SIGINT)

	// ssh.Handler 그대로 사용
	h := func(s ssh.Session) {
		ptyReq, winCh, isPty := s.Pty()
		if !isPty {
			fmt.Fprintln(s, "Error: PTY required. Reconnect with -t option.")
			_ = s.Exit(1)
			return
		}

		reader := bufio.NewReader(s)

		remote := s.RemoteAddr().String()
		ip := remote
		if host, _, err := net.SplitHostPort(remote); err == nil {
			ip = host
		}

		if banManager.IsBanned(ip) {
			fmt.Fprintln(s, "Your IP is banned.")
			_ = s.Exit(1)
			return
		}

		if !rateLimiter.CheckAndRecord(ip) {
			log.Printf("Banning IP %s for too many connections.", ip)
			banManager.Ban(ip)
			disconnected := globalChat.DisconnectByIP(ip)
			log.Printf("Disconnected %d existing session(s) from %s.", disconnected, ip)
			fmt.Fprintln(s, "Your IP is banned for creating too many connections.")
			_ = s.Exit(1)
			return
		}

		nickname := strings.TrimSpace(s.User())
		if nickname == "" {
			nickname = generateGuestNickname()
		}
		if len([]rune(nickname)) > 10 {
			nickname = string([]rune(nickname)[:10])
		}

		client := NewClient(globalChat, s, nickname, int(ptyReq.Window.Width), int(ptyReq.Window.Height), ip)
		globalChat.AddClient(client)
		defer func() {
			globalChat.RemoveClient(client)
			client.Close()
			globalChat.AppendSystemMessage(fmt.Sprintf("%s left the chat", nickname))
		}()

		fmt.Fprint(s, "\x1b[2J\x1b[H")
		globalChat.AppendSystemMessage(fmt.Sprintf("%s joined the chat", nickname))

		go client.MonitorWindow(winCh)
		client.Start(reader, s.Context())
		client.Wait()
	}

	// 서버를 객체로 만들어서 Close 할 수 있게
	srv := &ssh.Server{
		Addr:    ":2222",
		Handler: h,
	}
	srv.SetOption(ssh.HostKeyFile("host.key"))

	// 서버 실행은 고루틴에서; log.Fatal 쓰지 마세요
	go func() {
		log.Println("starting ssh chat server on port 2222...")
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, net.ErrClosed) {
			// 여기서 종료하지 않음
			log.Printf("ssh server error: %v", err)
			quitCh <- os.Interrupt
		}
	}()

	// 메인 고루틴은 신호 대기 → 카운트다운 → 서버 종료
	<-quitCh

	globalChat.AppendSystemMessage("서버 폭파 5초전")
	for i := 5; i >= 0; i-- {
		time.Sleep(time.Second)
		globalChat.AppendSystemMessage(fmt.Sprintf("%d 초", i))
	}
	globalChat.AppendSystemMessage("💥💥💥💥💥")
	globalChat.AppendSystemMessage("아마 관리자가 부지런하면 금방 복구할꺼에요.")
	globalChat.AppendSystemMessage("💥💥💥💥💥")
	time.Sleep(3 * time.Second)
	globalChat.AppendSystemMessage("뭐야 왜 안터져")
	time.Sleep(4 * time.Second)
	globalChat.AppendSystemMessage("???")
	time.Sleep(time.Second)
	globalChat.AppendSystemMessage("Control + C")
	time.Sleep(time.Second)
	globalChat.AppendSystemMessage("????????????")
	time.Sleep(500 * time.Millisecond)

	// 새 연결 막고 종료
	_ = srv.Close()
	os.Exit(0)
}

// 범위 기반(명시적 블록) 체크를 추가로 하고 싶다면 아래도 사용
func isCombiningBlock(r rune) bool {
	switch {
	case r >= 0x0300 && r <= 0x036F: // Combining Diacritical Marks
		return true
	case r >= 0x1AB0 && r <= 0x1AFF: // Combining Diacritical Marks Extended
		return true
	case r >= 0x1DC0 && r <= 0x1DFF: // Combining Diacritical Marks Supplement
		return true
	case r >= 0x20D0 && r <= 0x20FF: // Combining Diacritical Marks for Symbols
		return true
	case r >= 0xFE20 && r <= 0xFE2F: // Combining Half Marks
		return true
	default:
		return false
	}
}

func isBlockedRune(r rune) bool {
	// 범주 기반(Mn/Me) + 범위 기반을 모두 허용
	if unicode.Is(unicode.Mn, r) || unicode.Is(unicode.Me, r) {
		return true
	}
	return isCombiningBlock(r)
}

// extractMentions finds all @username mentions in a message
func extractMentions(text string) []string {
	var mentions []string
	words := strings.Fields(text)

	for _, word := range words {
		if strings.HasPrefix(word, "@") {
			// Remove @ and any trailing punctuation
			mention := strings.TrimPrefix(word, "@")
			mention = strings.TrimFunc(mention, func(r rune) bool {
				return !unicode.IsLetter(r) && !unicode.IsDigit(r) && r != '_'
			})
			if mention != "" {
				mentions = append(mentions, mention)
			}
		}
	}

	return mentions
}

// highlightMentions adds highlighting to mentioned usernames in the message text
func highlightMentions(text string, mentions []string) string {
	if len(mentions) == 0 {
		return text
	}

	result := text
	for _, mention := range mentions {
		// Create patterns for @username and @username with punctuation
		pattern := "@" + mention
		highlighted := fmt.Sprintf("\x1b[1;33m%s\x1b[0m", pattern) // Bold yellow
		result = strings.ReplaceAll(result, pattern, highlighted)

		// Also handle case where mention might have punctuation after it
		patterns := []string{
			"@" + mention + ",",
			"@" + mention + ".",
			"@" + mention + "!",
			"@" + mention + "?",
			"@" + mention + ":",
			"@" + mention + ";",
		}

		for _, p := range patterns {
			if strings.Contains(result, p) {
				// Find the index and replace with highlighted version plus punctuation
				parts := strings.SplitN(p, "@"+mention, 2)
				if len(parts) == 2 {
					highlightedWithPunct := fmt.Sprintf("\x1b[1;33m@%s\x1b[0m%s", mention, parts[1])
					result = strings.ReplaceAll(result, p, highlightedWithPunct)
				}
			}
		}
	}

	return result
}

func ValidateNoCombining(input string) error {
	// 혹시 모를 누락을 대비해 룬 단위로 다시 점검(보수적)
	for _, r := range input {
		if isBlockedRune(r) {
			return errors.New("input contains combining diacritical marks (blocked)")
		}
	}
	return nil
}
