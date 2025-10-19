package main

import (
	"bufio"
	"context"
	"crypto/ecdsa"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/asn1"
	"encoding/binary"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"log"
	"math/big"
	"math/rand"
	"net"
	"os"
	"os/signal"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
	"unicode"

	"github.com/gliderlabs/ssh"
	pb "github.com/iwanhae/ssh-chat/proto"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/keepalive"
	"google.golang.org/grpc/peer"
)

type Message struct {
	ID       string // Unique message ID for tracking streaming updates
	Time     time.Time
	Nick     string
	Text     string
	Color    int
	IP       string
	Mentions []string // List of mentioned usernames
	IsUpdate bool     // True if this is an update to an existing message
}

type ChatServer struct {
	mu        sync.RWMutex
	messages  []Message
	clients   map[*Client]struct{}
	ipCounts  map[string]int  // Track connections per IP
	nicknames map[string]bool // Track used nicknames
}

var (
	globalChat   = NewChatServer()
	guestCounter uint64
	grpcService  *gRPCServer
)

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

var banManager = NewBanManager()

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
	// Generate ID if not set
	if msg.ID == "" {
		msg.ID = fmt.Sprintf("%d-%s", time.Now().UnixNano(), msg.Nick)
	}

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

func (cs *ChatServer) AppendMessageWithID(msg Message) string {
	// Generate ID if not set
	if msg.ID == "" {
		msg.ID = fmt.Sprintf("%d-%s", time.Now().UnixNano(), msg.Nick)
	}
	cs.AppendMessage(msg)
	return msg.ID
}

// UpdateMessage updates an existing message (for streaming AI responses)
func (cs *ChatServer) UpdateMessage(id string, text string) bool {
	trimmed := strings.TrimLeftFunc(text, unicode.IsSpace)
	trimmed = strings.ReplaceAll(trimmed, "\r", "")
	trimmed = strings.ReplaceAll(trimmed, "\n", " ")

	cs.mu.Lock()

	var (
		clients []*Client
		updated bool
	)

	for i := len(cs.messages) - 1; i >= 0; i-- {
		if cs.messages[i].ID == id {
			cs.messages[i].Text = trimmed
			cs.messages[i].IsUpdate = true

			clients = make([]*Client, 0, len(cs.clients))
			for c := range cs.clients {
				clients = append(clients, c)
			}
			updated = true
			break
		}
	}

	cs.mu.Unlock()

	if updated {
		for _, client := range clients {
			client.NotifyWithBell(false)
		}
	}

	return updated
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

type Client struct {
	session ssh.Session
	server  *ChatServer

	mu                sync.Mutex
	writeMu           sync.Mutex // Protects writes to session
	width             int
	height            int
	scrollOffset      int
	inputBuffer       []rune
	messageTimestamps []time.Time

	updateCh  chan struct{}
	done      chan struct{}
	closeOnce sync.Once
	wg        sync.WaitGroup
	nickname  string
	color     int
	ip        string
}

var colors = []int{
	31, 32, 33, 34, 35, 36,
}

func NewClient(server *ChatServer, session ssh.Session, nickname string, width, height int, ip string) *Client {
	if width <= 0 || width > 8192 {
		width = 80
	}
	if height <= 0 || height > 8192 {
		height = 24
	}
	return &Client{
		session:           session,
		server:            server,
		width:             width,
		height:            height,
		updateCh:          make(chan struct{}, 16),
		done:              make(chan struct{}),
		nickname:          nickname,
		color:             colors[rand.Intn(len(colors))],
		inputBuffer:       make([]rune, 0, 128),
		messageTimestamps: make([]time.Time, 0),
		ip:                ip,
	}
}

func (c *Client) Start(reader *bufio.Reader, ctx context.Context) {
	c.wg.Add(2)
	go func() {
		defer c.wg.Done()
		c.renderLoop()
	}()
	go func() {
		defer c.wg.Done()
		c.inputLoop(reader)
	}()
	go func() {
		select {
		case <-ctx.Done():
			c.Close()
		case <-c.done:
		}
	}()
	c.Notify()
}

func (c *Client) Wait() {
	c.wg.Wait()
}

func (c *Client) Close() {
	c.closeOnce.Do(func() {
		close(c.done)
	})
}

func (c *Client) Notify() {
	for {
		select {
		case c.updateCh <- struct{}{}:
			return
		default:
			select {
			case <-c.updateCh:
			default:
			}
		}
	}
}

// NotifyWithBell sends a notification with optional bell character
func (c *Client) NotifyWithBell(withBell bool) {
	if withBell {
		// Send bell character before the update notification
		c.writeMu.Lock()
		c.session.Write([]byte("\a"))
		c.writeMu.Unlock()
	}
	c.Notify()
}

func (c *Client) SetWindowSize(width, height int) {
	c.mu.Lock()
	if width > 0 && width <= 8192 {
		c.width = width
	}
	if height > 0 && height <= 8192 {
		c.height = height
	}
	c.mu.Unlock()
	c.Notify()
}

func (c *Client) MonitorWindow(winCh <-chan ssh.Window) {
	for win := range winCh {
		c.SetWindowSize(win.Width, win.Height)
	}
	c.Close()
}

func (c *Client) renderLoop() {
	for {
		select {
		case <-c.updateCh:
			c.render()
		case <-c.done:
			return
		}
	}
}

func (c *Client) render() {
	allMessages := c.server.Messages()

	c.mu.Lock()
	width := c.width
	height := c.height
	scroll := c.scrollOffset
	inputCopy := append([]rune(nil), c.inputBuffer...)
	c.mu.Unlock()

	if width <= 0 {
		width = 80
	}
	if height <= 0 {
		height = 24
	}

	messageArea := height - 2
	if messageArea < 1 {
		messageArea = 1
	}

	// [OPTIMIZATION]
	// 필요한 라인만 생성합니다. 화면 영역(messageArea)과 스크롤 오프셋(scroll)을
	// 합친 만큼의 라인을 최신 메시지부터 역순으로 생성합니다.
	neededLines := messageArea + scroll
	var relevantLines []string

	// 전체 메시지를 역순으로 순회합니다.
	for i := len(allMessages) - 1; i >= 0; i-- {
		msg := allMessages[i]
		// 메시지 하나를 포맷팅하여 라인들로 변환합니다.
		msgLines := formatMessage(msg, width)

		// 생성된 라인들을 `relevantLines`의 앞쪽에 추가합니다.
		// 이렇게 하면 메시지 순서가 올바르게 유지됩니다.
		relevantLines = append(msgLines, relevantLines...)

		// 필요한 만큼의 라인이 모이면 더 이상 메시지를 처리하지 않고 루프를 종료합니다.
		if len(relevantLines) >= neededLines {
			break
		}
	}

	totalLines := len(relevantLines)
	maxOffset := 0
	if totalLines > messageArea {
		maxOffset = totalLines - messageArea
	}

	// 스크롤 오프셋이 최대치를 넘지 않도록 조정합니다.
	if scroll > maxOffset {
		scroll = maxOffset
		c.mu.Lock()
		c.scrollOffset = scroll
		c.mu.Unlock()
	}

	start := 0
	if totalLines > messageArea {
		start = totalLines - messageArea - scroll
	}
	end := start + messageArea
	if end > totalLines {
		end = totalLines
	}

	// 화면에 표시할 최종 라인들을 선택합니다.
	displayLines := relevantLines[start:end]

	status := fmt.Sprintf("Users:%d Messages:%d Scroll:%d/%d ↑/↓ to scroll", c.server.ClientCount(), len(allMessages), scroll, maxOffset)
	status = fitString(status, width)

	inputText := string(inputCopy)
	inputLimit := width - 2
	if inputLimit < 1 {
		inputLimit = width
	}
	inputText = tailString(inputText, inputLimit)

	var b strings.Builder
	b.Grow((messageArea + 3) * (width + 8))
	b.WriteString("\x1b[?25l")
	b.WriteString("\x1b[H")

	for i := 0; i < messageArea; i++ {
		b.WriteString("\x1b[2K")
		if i < len(displayLines) {
			b.WriteString(displayLines[i])
		}
		b.WriteByte('\n')
	}

	b.WriteString("\x1b[2K")
	b.WriteString(status)
	b.WriteByte('\n')

	b.WriteString("\x1b[2K")
	b.WriteString("> ")
	b.WriteString(inputText)
	b.WriteString("\x1b[K")
	b.WriteString("\x1b[?25h")

	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	if _, err := c.session.Write([]byte(b.String())); err != nil {
		c.Close()
	}
}

func (c *Client) inputLoop(reader *bufio.Reader) {
	for {
		r, _, err := reader.ReadRune()
		if err != nil {
			c.Close()
			return
		}

		switch r {
		case '\r':
			c.handleEnter()
		case '\n':
			// ignore bare line feeds; carriage return already handled
		case 127, '\b':
			c.handleBackspace()
		case 3: // Ctrl+C
			c.Close()
			return
		case 4: // Ctrl+D
			c.Close()
			return
		case '\x1b':
			c.handleEscape(reader)
		default:
			if !isControlRune(r) {
				c.handleRune(r)
			}
		}
	}
}

func (c *Client) handleEnter() {
	c.mu.Lock()
	text := strings.TrimSpace(string(c.inputBuffer))
	c.inputBuffer = c.inputBuffer[:0]
	c.scrollOffset = 0
	c.mu.Unlock()
	c.Notify()

	if text == "" {
		return
	}

	if err := ValidateNoCombining(text); err != nil {
		return
	}

	c.mu.Lock()
	now := time.Now()
	oneMinuteAgo := now.Add(-time.Minute)

	// Filter timestamps older than one minute
	n := 0
	for _, ts := range c.messageTimestamps {
		if ts.After(oneMinuteAgo) {
			c.messageTimestamps[n] = ts
			n++
		}
	}
	c.messageTimestamps = c.messageTimestamps[:n]

	// Add current message timestamp
	c.messageTimestamps = append(c.messageTimestamps, now)
	messageCount := len(c.messageTimestamps)
	c.mu.Unlock()

	if messageCount > 30 {
		log.Printf("Kicking client %s (%s) for spamming.", c.nickname, c.ip)
		banManager.Ban(c.ip)
		msg := fmt.Sprintf("야 `%s` 나가.", c.nickname)
		c.server.AppendSystemMessage(msg)
		c.session.Exit(1)
		c.Close()
		return
	}

	// Commands
	if strings.HasPrefix(text, "/ban ") {
		target := strings.TrimSpace(strings.TrimPrefix(text, "/ban "))
		// Allow just IP (IPv4/IPv6). No CIDR support for simplicity.
		if ip := net.ParseIP(target); ip == nil {
			c.server.AppendSystemMessage("Invalid IP address")
			return
		}
		banManager.Ban(target)
		disconnected := c.server.DisconnectByIP(target)
		c.server.AppendSystemMessage(fmt.Sprintf("IP %s banned. Disconnected %d session(s).", target, disconnected))
		return
	}

	msg := Message{
		Time:  time.Now(),
		Nick:  c.nickname,
		Text:  text,
		Color: c.color,
		IP:    c.ip,
	}
	c.server.AppendMessage(msg)

	// Broadcast to gRPC service if available
	if grpcService != nil {
		grpcService.BroadcastMessage(msg)
	}

}

func (c *Client) handleBackspace() {
	c.mu.Lock()
	if len(c.inputBuffer) > 0 {
		c.inputBuffer = c.inputBuffer[:len(c.inputBuffer)-1]
	}
	c.mu.Unlock()
	c.Notify()
}

func (c *Client) handleRune(r rune) {
	c.mu.Lock()
	c.inputBuffer = append(c.inputBuffer, r)
	c.mu.Unlock()
	c.Notify()
}

func (c *Client) handleEscape(reader *bufio.Reader) {
	b1, err := reader.ReadByte()
	if err != nil {
		c.Close()
		return
	}
	if b1 != '[' {
		return
	}
	b2, err := reader.ReadByte()
	if err != nil {
		c.Close()
		return
	}
	switch b2 {
	case 'A':
		c.mu.Lock()
		c.scrollOffset++
		c.mu.Unlock()
		c.Notify()
	case 'B':
		c.mu.Lock()
		if c.scrollOffset > 0 {
			c.scrollOffset--
		}
		c.mu.Unlock()
		c.Notify()
	}
}

func (c *Client) ClearScreen() {
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	fmt.Fprint(c.session, "\x1b[2J\x1b[H")
}

func isControlRune(r rune) bool {
	return r < 32 || r == 127
}

// [HELPER] O(n) 로직을 분리하기 위해, 메시지 '하나'만 포맷하는 헬퍼 함수를 만들었습니다.
func formatMessage(msg Message, width int) []string {
	color := msg.Color
	if color == 0 {
		color = 37 // default to white
	}
	coloredNick := fmt.Sprintf("\x1b[%dm%s\x1b[0m", color, msg.Nick)

	// Highlight mentions in the message text
	highlightedText := highlightMentions(msg.Text, msg.Mentions)

	prefix := fmt.Sprintf("[%s] %s: ", msg.Time.Format("15:04:05"), coloredNick)
	indent := strings.Repeat(" ", len(msg.Nick)+13)

	var lines []string
	segments := strings.Split(highlightedText, "\n")

	wrapWidth := width
	if msg.Nick == "AI" {
		// AI 메시지는 줄바꿈을 하지 않고 터미널이 처리하도록 매우 큰 너비를 줍니다.
		wrapWidth = 16384
	}

	for i, segment := range segments {
		base := segment
		if i == 0 {
			base = prefix + segment
		} else {
			base = indent + segment
		}
		wrapped := wrapString(base, wrapWidth)
		lines = append(lines, wrapped...)
	}
	return lines
}

func wrapString(s string, width int) []string {
	if width <= 0 {
		width = 80
	}
	runes := []rune(s)
	if len(runes) == 0 {
		return []string{""}
	}
	var result []string
	for len(runes) > 0 {
		// ANSI 이스케이프 코드를 고려한 너비 계산이 필요하지만, 간단하게 처리합니다.
		// 실제로는 더 복잡한 로직이 필요할 수 있습니다.
		// 여기서는 간단함을 위해 rune 개수로만 너비를 계산합니다.

		// 임시: 이스케이프 시퀀스를 무시하는 간단한 방법 (정확하지 않을 수 있음)
		var currentWidth int
		var breakIndex int = -1
		inEscape := false
		for i, r := range runes {
			if r == '\x1b' {
				inEscape = true
			}
			if !inEscape {
				currentWidth++
			}
			if r == 'm' && inEscape {
				inEscape = false
			}
			if currentWidth > width {
				breakIndex = i
				break
			}
		}

		if breakIndex == -1 {
			result = append(result, string(runes))
			break
		}

		// 단어 단위로 자르는 로직을 추가하면 더 좋습니다 (여기서는 글자 단위로 자름)
		if breakIndex > 0 {
			// 이스케이프 코드가 아닌 문자만 검사
			tempRunes := []rune{}
			inEscape = false
			for _, r := range runes[:breakIndex] {
				if r == '\x1b' {
					inEscape = true
				}
				if !inEscape {
					tempRunes = append(tempRunes, r)
				}
				if r == 'm' && inEscape {
					inEscape = false
				}
			}

			// 텍스트에서 마지막 공백 찾기
			realText := string(tempRunes)
			lastSpaceInText := strings.LastIndex(realText, " ")

			// 원본 rune 슬라이스에서 해당 공백 위치 찾기 (근사치)
			if lastSpaceInText != -1 {
				// 매우 단순화된 로직, 정확한 위치를 찾으려면 더 복잡한 파싱 필요
				// 여기서는 그냥 글자 단위로 자르는 것으로 대체
			}
		}

		result = append(result, string(runes[:breakIndex]))
		runes = runes[breakIndex:]
	}
	return result
}

func fitString(s string, width int) string {
	if width <= 0 {
		return s
	}
	runes := []rune(s)
	if len(runes) <= width {
		return s
	}
	return string(runes[:width])
}

func tailString(s string, width int) string {
	if width <= 0 {
		return s
	}
	runes := []rune(s)
	if len(runes) <= width {
		return s
	}
	return string(runes[len(runes)-width:])
}

// sanitizeNickname removes escape sequences and non-printable runes to keep terminal output safe.
func sanitizeNickname(raw string) string {
	var b strings.Builder
	b.Grow(len(raw))

	for _, r := range raw {
		if r == '\x1b' || r == '\x9b' {
			continue
		}
		if r < 0x20 || r == 0x7f {
			continue
		}
		if !unicode.IsPrint(r) {
			continue
		}
		b.WriteRune(r)
	}

	return strings.TrimSpace(b.String())
}

func generateGuestNickname() string {
	id := atomic.AddUint64(&guestCounter, 1)
	return fmt.Sprintf("guest-%d", id)
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

		// Check IP connection limit (max 2 per IP)
		if !globalChat.CheckIPLimit(ip) {
			fmt.Fprintln(s, "Connection limit exceeded for this IP (max 2 connections).")
			_ = s.Exit(1)
			return
		}

		nickname := sanitizeNickname(strings.TrimSpace(s.User()))
		if nickname == "" {
			nickname = generateGuestNickname()
		}
		if len([]rune(nickname)) > 10 {
			nickname = string([]rune(nickname)[:10])
		}
		nickname = sanitizeNickname(nickname)
		if nickname == "" {
			nickname = generateGuestNickname()
		}

		// Handle nickname collision by adding 8-character HEX suffix if needed
		finalNickname := globalChat.GetUniqueNickname(nickname)

		client := NewClient(globalChat, s, finalNickname, int(ptyReq.Window.Width), int(ptyReq.Window.Height), ip)
		globalChat.AddClient(client)
		defer func() {
			globalChat.RemoveClient(client)
			client.Close()
			globalChat.AppendSystemMessage(fmt.Sprintf("%s left the chat", finalNickname))
		}()

		client.ClearScreen()
		globalChat.AppendSystemMessage(fmt.Sprintf("%s joined the chat", finalNickname))

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

	// Start gRPC server
	grpcServer, grpcSvc, err := startGRPCServer(globalChat)
	if err != nil {
		log.Printf("Failed to start gRPC server: %v", err)
		log.Println("Continuing without gRPC AI integration...")
	} else {
		grpcService = grpcSvc
		grpcListener, err := net.Listen("tcp", ":3333")
		if err != nil {
			log.Fatalf("Failed to listen on port 3333: %v", err)
		}

		go func() {
			log.Println("starting gRPC AI server on port 3333...")
			if err := grpcServer.Serve(grpcListener); err != nil {
				log.Printf("gRPC server error: %v", err)
			}
		}()

		defer func() {
			log.Println("Stopping gRPC server...")
			grpcServer.GracefulStop()
		}()
	}

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
	time.Sleep(time.Second)
	globalChat.AppendSystemMessage("뭐야 왜 안터져")
	time.Sleep(time.Second)
	globalChat.AppendSystemMessage("???")
	time.Sleep(time.Second)
	globalChat.AppendSystemMessage("???????")
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

// loadClientPublicKey loads the ECDSA public key from PEM file
func loadClientPublicKey(filepath string) (*ecdsa.PublicKey, error) {
	keyData, err := os.ReadFile(filepath)
	if err != nil {
		return nil, fmt.Errorf("failed to read public key file: %v", err)
	}

	block, _ := pem.Decode(keyData)
	if block == nil {
		return nil, errors.New("failed to decode PEM block")
	}

	pub, err := x509.ParsePKIXPublicKey(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("failed to parse public key: %v", err)
	}

	ecdsaPub, ok := pub.(*ecdsa.PublicKey)
	if !ok {
		return nil, errors.New("not an ECDSA public key")
	}

	return ecdsaPub, nil
}

// verifySignature verifies ECDSA signature over IP address + timestamp
func verifySignature(pubKey *ecdsa.PublicKey, ipAddr string, timestamp int64, signature []byte) error {
	// Create message to verify (IP bytes + 8 bytes timestamp in big endian)
	// IP is in IPv4 format (e.g., "192.168.1.1")
	ipBytes := []byte(ipAddr)
	timestampBytes := make([]byte, 8)
	binary.BigEndian.PutUint64(timestampBytes, uint64(timestamp))

	// Concatenate IP + timestamp
	message := append(ipBytes, timestampBytes...)

	// Hash the message
	hash := sha256.Sum256(message)

	// Parse ASN.1 DER signature
	var sig struct {
		R, S *big.Int
	}
	if _, err := asn1.Unmarshal(signature, &sig); err != nil {
		return fmt.Errorf("failed to unmarshal signature: %v", err)
	}

	// Verify signature
	if !ecdsa.Verify(pubKey, hash[:], sig.R, sig.S) {
		return errors.New("signature verification failed")
	}

	// Check timestamp is recent (within 1 minute)
	now := time.Now().UnixMilli()
	timeDiff := now - timestamp
	if timeDiff < 0 {
		timeDiff = -timeDiff
	}
	if timeDiff > 1*60*1000 { // 1 minute in milliseconds
		return fmt.Errorf("timestamp too old or too far in future: %d ms", timeDiff)
	}

	return nil
}

// gRPCServer implements the StreamMiddleware service
type gRPCServer struct {
	pb.UnimplementedStreamMiddlewareServer
	chatServer     *ChatServer
	activeStreamMu sync.Mutex
	activeStreams  map[string]chan *pb.ChatMessage
	clientPubKey   *ecdsa.PublicKey // Client's public key for authentication
}

func newGRPCServer(cs *ChatServer) *gRPCServer {
	// Load client's public key for authentication
	pubKey, err := loadClientPublicKey("ai_grpc_client.pub")
	if err != nil {
		log.Fatalf("Failed to load client public key: %v", err)
	}
	log.Println("Loaded client public key for authentication")

	return &gRPCServer{
		chatServer:    cs,
		activeStreams: make(map[string]chan *pb.ChatMessage),
		clientPubKey:  pubKey,
	}
}

// StreamChat implements bidirectional streaming
func (s *gRPCServer) StreamChat(stream pb.StreamMiddleware_StreamChatServer) error {
	streamID := fmt.Sprintf("stream-%d", time.Now().UnixNano())
	log.Printf("New gRPC stream connected: %s", streamID)

	// Extract client IP from gRPC peer info
	p, ok := peer.FromContext(stream.Context())
	if !ok {
		return errors.New("failed to get peer from context")
	}
	clientAddr := p.Addr.String()
	// Extract IP without port (e.g., "192.168.1.1:12345" -> "192.168.1.1")
	clientIP := clientAddr
	if host, _, err := net.SplitHostPort(clientAddr); err == nil {
		clientIP = host
	}
	log.Printf("Client IP from peer: %s", clientIP)

	// Create channel for this stream
	streamChan := make(chan *pb.ChatMessage, 100)
	s.activeStreamMu.Lock()
	s.activeStreams[streamID] = streamChan
	s.activeStreamMu.Unlock()

	defer func() {
		s.activeStreamMu.Lock()
		delete(s.activeStreams, streamID)
		s.activeStreamMu.Unlock()
		close(streamChan)
		log.Printf("gRPC stream disconnected: %s", streamID)
	}()

	// Authentication flag
	authenticated := false

	// Goroutine to receive AI responses from client
	errChan := make(chan error, 1)
	go func() {
		for {
			resp, err := stream.Recv()
			if err == io.EOF {
				log.Printf("Client closed stream: %s", streamID)
				errChan <- nil
				return
			}
			if err != nil {
				log.Printf("Error receiving AI response: %v", err)
				errChan <- err
				return
			}

			// Verify authentication on first message
			if !authenticated {
				if len(resp.AuthSignature) == 0 || resp.AuthTimestamp == 0 {
					log.Printf("Authentication failed: missing signature or timestamp")
					errChan <- errors.New("authentication required")
					return
				}

				// Check if IP in message matches peer IP
				if resp.Ip == "" {
					log.Printf("Authentication failed: missing IP in message")
					errChan <- errors.New("IP address required in auth message")
					return
				}

				// Allow localhost connections to bypass IP matching check
				isLocalhost := clientIP == "127.0.0.1" || clientIP == "::1" ||
					clientIP == "localhost" || strings.HasPrefix(clientIP, "[::1]")

				if !isLocalhost && resp.Ip != clientIP {
					log.Printf("Authentication failed: IP mismatch - message IP: %s, peer IP: %s", resp.Ip, clientIP)
					errChan <- fmt.Errorf("IP address mismatch: expected %s, got %s", clientIP, resp.Ip)
					return
				}

				if isLocalhost {
					log.Printf("Localhost connection detected, bypassing IP match check (peer IP: %s)", clientIP)
				}

				// Verify signature over IP + timestamp
				if err := verifySignature(s.clientPubKey, resp.Ip, resp.AuthTimestamp, resp.AuthSignature); err != nil {
					log.Printf("Authentication failed: %v", err)
					errChan <- fmt.Errorf("authentication failed: %v", err)
					return
				}

				authenticated = true
				log.Printf("Client authenticated successfully for stream %s (IP: %s)", streamID, clientIP)

				// Skip processing if this is an auth-only message (empty message_id and text)
				if resp.MessageId == "" && resp.Text == "" {
					continue
				}
			}

			// Update or append AI message (skip empty messages)
			if resp.MessageId != "" && resp.Text != "" {
				// Use nick from response, default to "AI" if not set
				nick := resp.Nick
				if nick == "" {
					nick = "AI"
				}

				if !s.chatServer.UpdateMessage(resp.MessageId, resp.Text) {
					aiMsg := Message{
						ID:    resp.MessageId,
						Time:  time.Now(),
						Nick:  nick,
						Text:  resp.Text, // stream-accumulated message
						Color: 94,
					}
					s.chatServer.AppendMessage(aiMsg)
				}
			}
		}
	}()

	// Send chat messages to client
	for {
		select {
		case msg, ok := <-streamChan:
			if !ok {
				return nil
			}
			if err := stream.Send(msg); err != nil {
				log.Printf("Error sending message to AI: %v", err)
				return err
			}
		case err := <-errChan:
			return err
		}
	}
}

// BroadcastMessage sends a message to all active gRPC streams
func (s *gRPCServer) BroadcastMessage(msg Message) {
	if msg.Nick == "AI" || msg.Nick == "server" {
		return // Don't send AI or server messages back to AI
	}

	grpcMsg := &pb.ChatMessage{
		MessageId: msg.ID,
		Nick:      msg.Nick,
		Text:      msg.Text,
		Timestamp: msg.Time.Unix(),
		Ip:        msg.IP,
	}

	s.activeStreamMu.Lock()
	defer s.activeStreamMu.Unlock()

	for _, streamChan := range s.activeStreams {
		select {
		case streamChan <- grpcMsg:
		default:
			// Channel full, skip
		}
	}
}

// startGRPCServer starts the gRPC server with TLS only (no client cert verification)
func startGRPCServer(cs *ChatServer) (*grpc.Server, *gRPCServer, error) {
	// Load server certificate and key
	// Using grpc_server_cert.pub and host.key (self-signed for development)
	cert, err := tls.LoadX509KeyPair("grpc_server_cert.pub", "host.key")
	if err != nil {
		return nil, nil, fmt.Errorf("failed to load server cert: %v", err)
	}

	// Create TLS config without client certificate verification
	tlsConfig := &tls.Config{
		Certificates: []tls.Certificate{cert},
		ClientAuth:   tls.NoClientCert, // Changed from RequireAndVerifyClientCert
	}

	// Keepalive settings for 24/7 connection stability
	kaep := grpc.KeepaliveEnforcementPolicy(keepalive.EnforcementPolicy{
		MinTime:             5 * time.Second, // Minimum time client should wait before sending keepalive ping
		PermitWithoutStream: true,            // Allow pings even when there are no active streams
	})

	kasp := grpc.KeepaliveParams(keepalive.ServerParameters{
		MaxConnectionIdle:     9999 * time.Minute, // Max time connection can be idle before server sends GOAWAY
		MaxConnectionAge:      0,                  // Infinite - no max connection age
		MaxConnectionAgeGrace: 0,                  // Infinite grace period
		Time:                  30 * time.Second,   // Send keepalive ping every 30 seconds
		Timeout:               10 * time.Second,   // Wait 10 seconds for keepalive response
	})

	grpcServer := grpc.NewServer(
		grpc.Creds(credentials.NewTLS(tlsConfig)),
		kaep,
		kasp,
	)

	streamMiddlewareService := newGRPCServer(cs)
	pb.RegisterStreamMiddlewareServer(grpcServer, streamMiddlewareService)

	return grpcServer, streamMiddlewareService, nil
}
