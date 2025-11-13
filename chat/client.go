package chat

import (
	"bufio"
	"context"
	"database/sql" // Add this line
	"errors"
	"fmt"
	"log"
	"net"
	"strings"
	"sync"
	"time"
	"unicode"

	"github.com/gliderlabs/ssh"
)

type Client struct {
	session    ssh.Session
	server     *ChatServer
	banManager *BanManager
	store      MessageStore // Add this line

	mu                sync.Mutex
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

func NewClient(server *ChatServer, banManager *BanManager, store MessageStore, session ssh.Session, nickname string, width, height, color int, ip string) *Client {
	if width <= 0 || width > 8192 {
		width = 80
	}
	if height <= 0 || height > 8192 {
		height = 24
	}

	// Load user color from store, or create if new
	userColor, err := store.GetUserColor(nickname)
	if err != nil {
		if err == sql.ErrNoRows {
			// New user, create with default color
			userColor = 37 // Default to white
			if err := store.CreateUser(nickname, userColor); err != nil {
				log.Printf("Failed to create new user %s in DB: %v", nickname, err)
			}
		} else {
			log.Printf("Failed to get user color for %s from DB: %v. Using default color.", nickname, err)
			userColor = 37 // Fallback to default color on error
		}
	}
	// If the color passed in is not 0, it means it was explicitly set (e.g., by a command line arg),
	// so we should prioritize that over the stored color.
	if color != 0 {
		userColor = color
	}

	return &Client{
		session:           session,
		server:            server,
		banManager:        banManager,
		store:             store,
		width:             width,
		height:            height,
		updateCh:          make(chan struct{}, 16),
		done:              make(chan struct{}),
		nickname:          nickname,
		color:             userColor, // Use the loaded/default color
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
	select {
	case c.updateCh <- struct{}{}:
	default:
	}
}

// NotifyWithBell sends a notification with optional bell character
func (c *Client) NotifyWithBell(withBell bool) {
	if withBell {
		// Send bell character before the update notification
		c.session.Write([]byte("\a"))
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

	status := fmt.Sprintf("Users:%d Messages:%d Scroll:%d/%d ↑/↓, PgUp/PgDn to scroll", c.server.ClientCount(), len(allMessages), scroll, maxOffset)
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
	oneMinuteAgo := time.Now().Add(-time.Minute)

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
	c.messageTimestamps = append(c.messageTimestamps, time.Now())
	messageCount := len(c.messageTimestamps)
	c.mu.Unlock()

	if messageCount > 30 {
		log.Printf("Kicking client %s (%s) for spamming.", c.nickname, c.ip)
		c.banManager.Ban(c.ip)
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
		c.banManager.Ban(target)
		disconnected := c.server.DisconnectByIP(target)
		c.server.AppendSystemMessage(fmt.Sprintf("IP %s banned. Disconnected %d session(s).", target, disconnected))
		return
	} else if strings.HasPrefix(text, "@색 ") { // Check for @색 command
		parts := strings.SplitN(text, " ", 2)
		if len(parts) == 2 {
			c.handleColorCommand(parts[1])
		} else {
			c.server.AppendSystemMessage("사용법: @색 [색상명]")
		}
		return // Important: return after handling the command
	}

	c.server.AppendMessage(Message{
		Time:  getTimeInSeoul(),
		Nick:  c.nickname,
		Text:  text,
		Color: c.color,
		IP:    c.ip,
	})

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
	case '5': // PgUp
		b3, err := reader.ReadByte()
		if err != nil || b3 != '~' {
			return
		}
		c.mu.Lock()
		c.scrollOffset += 100
		c.mu.Unlock()
		c.Notify()
	case '6': // PgDown
		b3, err := reader.ReadByte()
		if err != nil || b3 != '~' {
			return
		}
		c.mu.Lock()
		c.scrollOffset -= 100
		if c.scrollOffset < 0 {
			c.scrollOffset = 0
		}
		c.mu.Unlock()
		c.Notify()
	}
}

func isControlRune(r rune) bool {
	return r < 32 || r == 127
}

// handleColorCommand processes the @색 command, updates the client's color, and persists it.
func (c *Client) handleColorCommand(colorName string) {
	newColor := 37 // Default to white

	// Determine the ANSI color code based on colorName
	if colorName == "빨강" || strings.Contains(colorName, "빨") {
		newColor = 31
	} else if colorName == "녹색" || strings.Contains(colorName, "초") {
		newColor = 32
	} else if strings.Contains(colorName, "노") || strings.Contains(colorName, "갈") {
		newColor = 33
	} else if strings.Contains(colorName, "파") {
		newColor = 34
	} else if strings.Contains(colorName, "자") || strings.Contains(colorName, "분") {
		newColor = 35
	} else if strings.Contains(colorName, "하늘") || strings.Contains(colorName, "청록") {
		newColor = 36
	}

	// Update client's current color
	c.mu.Lock()
	c.color = newColor
	c.mu.Unlock()

	// Persist the new color to the database
	if err := c.store.SetUserColor(c.nickname, newColor); err != nil {
		log.Printf("Failed to persist color for user %s: %v", c.nickname, err)
		c.server.AppendSystemMessage(fmt.Sprintf("Failed to save your color preference, %s.", c.nickname))
		return
	}

	c.server.AppendSystemMessage(fmt.Sprintf("%s님의 색상이 변경되었습니다.", c.nickname))
	c.Notify()
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
	for i, segment := range segments {
		base := segment
		if i == 0 {
			base = prefix + segment
		} else {
			base = indent + segment
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
