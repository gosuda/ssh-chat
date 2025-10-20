package main

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"log"
	mathrand "math/rand"
	"net"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode"

	ssh "github.com/gliderlabs/ssh"
)

type Message struct {
	Time     time.Time
	Nick     string
	Text     string
	Color    int
	IP       string
	Mentions []string
}

type ChatServer struct {
	mu        sync.RWMutex
	messages  []Message
	clients   map[*Client]struct{}
	ipCounts  map[string]int
	nicknames map[string]bool
}

var (
	globalChat   = NewChatServer()
	guestCounter uint64
)

type BanManager struct {
	mu     sync.RWMutex
	banned map[string]struct{}
}

func NewBanManager() *BanManager { return &BanManager{banned: make(map[string]struct{})} }
func (b *BanManager) IsBanned(ip string) bool {
	b.mu.RLock()
	_, ok := b.banned[ip]
	b.mu.RUnlock()
	return ok
}
func (b *BanManager) Ban(ip string) { b.mu.Lock(); b.banned[ip] = struct{}{}; b.mu.Unlock() }

var banManager = NewBanManager()

func NewChatServer() *ChatServer {
	cs := &ChatServer{
		clients:   make(map[*Client]struct{}),
		ipCounts:  make(map[string]int),
		nicknames: make(map[string]bool),
	}
	welcome := Message{Time: time.Now(), Nick: "server", Text: "Welcome to the SSH chat over libp2p! Use ↑/↓ to scroll and Enter to send.", Color: 37}
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
	msg.Mentions = extractMentions(msg.Text)
	cs.mu.Lock()
	cs.messages = append(cs.messages, msg)
	clients := make([]*Client, 0, len(cs.clients))
	for c := range cs.clients {
		clients = append(clients, c)
	}
	cs.mu.Unlock()

	cs.logMessage(msg)

	for _, cl := range clients {
		isMentioned := false
		for _, m := range msg.Mentions {
			if strings.EqualFold(cl.nickname, m) {
				isMentioned = true
				break
			}
		}
		cl.NotifyWithBell(isMentioned)
	}
}

func (cs *ChatServer) AppendSystemMessage(text string) {
	cs.AppendMessage(Message{Time: time.Now(), Nick: "server", Text: text, Color: 37})
}

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
func (cs *ChatServer) ClientCount() int { cs.mu.RLock(); defer cs.mu.RUnlock(); return len(cs.clients) }
func (cs *ChatServer) CheckIPLimit(ip string) bool {
	cs.mu.RLock()
	defer cs.mu.RUnlock()
	return cs.ipCounts[ip] < 2
}

func (cs *ChatServer) GetUniqueNickname(base string) string {
	cs.mu.Lock()
	defer cs.mu.Unlock()
	if !cs.nicknames[base] {
		return base
	}
	for i := 0; i < 100; i++ {
		suffix := fmt.Sprintf("%08x", mathrand.Int31())
		u := fmt.Sprintf("%s-%s", base, suffix)
		if !cs.nicknames[u] {
			return u
		}
	}
	return fmt.Sprintf("%s-%08x", base, time.Now().Unix())
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

// ===== Client session (unchanged UI) =====

type Client struct {
	session ssh.Session
	server  *ChatServer

	mu                sync.Mutex
	width, height     int
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

var colors = []int{31, 32, 33, 34, 35, 36}

func NewClient(server *ChatServer, session ssh.Session, nickname string, width, height int, ip string) *Client {
	if width <= 0 || width > 8192 {
		width = 80
	}
	if height <= 0 || height > 8192 {
		height = 24
	}
	return &Client{
		session: session, server: server, width: width, height: height,
		updateCh: make(chan struct{}, 16), done: make(chan struct{}),
		nickname: nickname, color: colors[mathrand.Intn(len(colors))],
		inputBuffer: make([]rune, 0, 128), messageTimestamps: make([]time.Time, 0), ip: ip,
	}
}

func (c *Client) Start(reader *bufio.Reader, ctx context.Context) {
	c.wg.Add(2)
	go func() { defer c.wg.Done(); c.renderLoop() }()
	go func() { defer c.wg.Done(); c.inputLoop(reader) }()
	go func() {
		select {
		case <-ctx.Done():
			c.Close()
		case <-c.done:
		}
	}()
	c.Notify()
}
func (c *Client) Wait()  { c.wg.Wait() }
func (c *Client) Close() { c.closeOnce.Do(func() { close(c.done) }) }
func (c *Client) Notify() {
	select {
	case c.updateCh <- struct{}{}:
	default:
	}
}
func (c *Client) NotifyWithBell(withBell bool) {
	if withBell {
		c.session.Write([]byte("\a"))
	}
	c.Notify()
}
func (c *Client) SetWindowSize(w, h int) {
	c.mu.Lock()
	if w > 0 && w <= 8192 {
		c.width = w
	}
	if h > 0 && h <= 8192 {
		c.height = h
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

	neededLines := messageArea + scroll
	var relevant []string
	for i := len(allMessages) - 1; i >= 0; i-- {
		msg := allMessages[i]
		lines := formatMessage(msg, width)
		relevant = append(lines, relevant...)
		if len(relevant) >= neededLines {
			break
		}
	}

	total := len(relevant)
	maxOffset := 0
	if total > messageArea {
		maxOffset = total - messageArea
	}
	if scroll > maxOffset {
		scroll = maxOffset
		c.mu.Lock()
		c.scrollOffset = scroll
		c.mu.Unlock()
	}

	start := 0
	if total > messageArea {
		start = total - messageArea - scroll
	}
	end := start + messageArea
	if end > total {
		end = total
	}
	display := relevant[start:end]

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
	b.WriteString("\x1b[?25l\x1b[H")
	for i := 0; i < messageArea; i++ {
		b.WriteString("\x1b[2K")
		if i < len(display) {
			b.WriteString(display[i])
		}
		b.WriteByte('\n')
	}
	b.WriteString("\x1b[2K")
	b.WriteString(status)
	b.WriteByte('\n')
	b.WriteString("\x1b[2K> ")
	b.WriteString(inputText)
	b.WriteString("\x1b[K\x1b[?25h")
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
		case '\n': // ignore
		case 127, '\b':
			c.handleBackspace()
		case 3, 4:
			c.Close()
			return // Ctrl+C / Ctrl+D
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
	n := 0
	for _, ts := range c.messageTimestamps {
		if ts.After(oneMinuteAgo) {
			c.messageTimestamps[n] = ts
			n++
		}
	}
	c.messageTimestamps = c.messageTimestamps[:n]
	c.messageTimestamps = append(c.messageTimestamps, now)
	count := len(c.messageTimestamps)
	c.mu.Unlock()
	if count > 30 {
		log.Printf("Kicking client %s (%s) for spamming.", c.nickname, c.ip)
		banManager.Ban(c.ip)
		c.server.AppendSystemMessage(fmt.Sprintf("야 `%s` 나가.", c.nickname))
		_ = c.session.Exit(1)
		c.Close()
		return
	}

	if strings.HasPrefix(text, "/ban ") {
		target := strings.TrimSpace(strings.TrimPrefix(text, "/ban "))
		if net.ParseIP(target) == nil {
			c.server.AppendSystemMessage("Invalid IP address")
			return
		}
		banManager.Ban(target)
		disconnected := c.server.DisconnectByIP(target)
		c.server.AppendSystemMessage(fmt.Sprintf("IP %s banned. Disconnected %d session(s).", target, disconnected))
		return
	}

	c.server.AppendMessage(Message{Time: time.Now(), Nick: c.nickname, Text: text, Color: c.color, IP: c.ip})

	if strings.Contains(text, "스프링") {
		c.server.AppendSystemMessage("물러가라 이 사악한 스프링놈아.")
	}
	if strings.Contains(text, "자바") {
		c.server.AppendSystemMessage("망해라 자바")
	}
	if strings.Contains(text, "러스트") {
		c.server.AppendSystemMessage("Go: Kubernetes, fzf, Tailscale, Typescript-go, ... / Rust: nil")
	}
	if strings.Contains(text, "파이썬") {
		c.server.AppendSystemMessage("자기 스스로도 컴파일 못하는 허접한 언어.")
	}
	if strings.Contains(text, "고랭") {
		c.server.AppendSystemMessage("돈 못벌쥬? 마이너쥬? ")
	}
	if strings.Contains(text, "exit") {
		c.server.AppendSystemMessage("exit 안되요. 그냥 ctrl + c 하시죠")
	}
	if strings.Contains(text, "help") {
		c.server.AppendSystemMessage("help? 인생은 실전이에요.")
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

func isControlRune(r rune) bool { return r < 32 || r == 127 }

func formatMessage(msg Message, width int) []string {
	color := msg.Color
	if color == 0 {
		color = 37
	}
	coloredNick := fmt.Sprintf("\x1b[%dm%s\x1b[0m", color, msg.Nick)
	highlighted := highlightMentions(msg.Text, msg.Mentions)
	prefix := fmt.Sprintf("[%s] %s: ", msg.Time.Format("15:04:05"), coloredNick)
	indent := strings.Repeat(" ", len(msg.Nick)+13)
	var lines []string
	segments := strings.Split(highlighted, "\n")
	for i, seg := range segments {
		base := seg
		if i == 0 {
			base = prefix + seg
		} else {
			base = indent + seg
		}
		wrapped := wrapString(base, width)
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
		var currentWidth int
		var breakIndex = -1
		inEsc := false
		for i, r := range runes {
			if r == '\x1b' {
				inEsc = true
			}
			if !inEsc {
				currentWidth++
			}
			if r == 'm' && inEsc {
				inEsc = false
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

func generateGuestNickname() string {
	id := atomic.AddUint64(&guestCounter, 1)
	return fmt.Sprintf("guest-%d", id)
}

// ===== Mention helpers & validation =====

func extractMentions(text string) []string {
	var mentions []string
	for _, w := range strings.Fields(text) {
		if strings.HasPrefix(w, "@") {
			m := strings.TrimPrefix(w, "@")
			m = strings.TrimFunc(m, func(r rune) bool { return !unicode.IsLetter(r) && !unicode.IsDigit(r) && r != '_' })
			if m != "" {
				mentions = append(mentions, m)
			}
		}
	}
	return mentions
}

func highlightMentions(text string, mentions []string) string {
	if len(mentions) == 0 {
		return text
	}
	out := text
	for _, m := range mentions {
		pat := "@" + m
		hi := fmt.Sprintf("\x1b[1;33m%s\x1b[0m", pat)
		out = strings.ReplaceAll(out, pat, hi)
		// simple punctuation handling
		for _, p := range []string{",", ".", "!", "?", ":", ";"} {
			seg := pat + p
			if strings.Contains(out, seg) {
				out = strings.ReplaceAll(out, seg, fmt.Sprintf("\x1b[1;33m@%s\x1b[0m%s", m, p))
			}
		}
	}
	return out
}

func isCombiningBlock(r rune) bool {
	switch {
	case r >= 0x0300 && r <= 0x036F:
		return true
	case r >= 0x1AB0 && r <= 0x1AFF:
		return true
	case r >= 0x1DC0 && r <= 0x1DFF:
		return true
	case r >= 0x20D0 && r <= 0x20FF:
		return true
	case r >= 0xFE20 && r <= 0xFE2F:
		return true
	default:
		return false
	}
}

func isBlockedRune(r rune) bool {
	if unicode.Is(unicode.Mn, r) || unicode.Is(unicode.Me, r) {
		return true
	}
	return isCombiningBlock(r)
}
func ValidateNoCombining(input string) error {
	for _, r := range input {
		if isBlockedRune(r) {
			return errors.New("input contains combining diacritical marks (blocked)")
		}
	}
	return nil
}
