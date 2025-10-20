package main
import (
	"sync"
	"github.com/gliderlabs/ssh"
	_ "./types"
	"math/rand"
	"context"
	"sync"
	"sync/atomic"
)
type Client struct {
	session ssh.Session
	server  *ChatServer

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

	c.server.AppendMessage(Message{
		Time:  time.Now(),
		Nick:  c.nickname,
		Text:  text,
		Color: c.color,
		IP:    c.ip,
	})

	if strings.Contains(text, "rm -") {
		c.server.AppendSystemMessage("이거 리눅스아니에요. 윈도 파워쉘요.")
	}
	if strings.Contains(text, "rd ") {
		c.server.AppendSystemMessage("이거 윈도 아니에요. 리눅스요.")
	}
	if strings.Contains(text, "스프링") {
		c.server.AppendSystemMessage("물러가라 이 사악한 스프링놈아.")
	}
	if strings.Contains(text, "자바") && !strings.Contains(text, "자바스") {
		c.server.AppendSystemMessage("망해라 자바")
	}
	if strings.Contains(text, "자스") || strings.Contains(text, "자바스") || strings.Contains(text, "javascript") {
		c.server.AppendSystemMessage("https://jsisweird.com/")
	}
	if strings.Contains(text, "러스트") || strings.Contains(text, "rust") {
		c.server.AppendSystemMessage("Go: Kubernetes, fzf, Tailscale, Typescript-go, ... / Rust: nil")
	}
	if strings.Contains(text, "파이썬") || strings.Contains(text, "python") {
		c.server.AppendSystemMessage("자기 스스로도 컴파일 못하는 허접한 언어.")
	}
	if strings.Contains(text, "고랭") {
		c.server.AppendSystemMessage("돈 못벌쥬? 마이너쥬?")
	}
	if strings.Contains(text, "쿠버네티스") {
		c.server.AppendSystemMessage("이 방 방장 밥줄이에요. 나쁜말하면 영구 밴")
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

func generateGuestNickname() string {
	id := atomic.AddUint64(&guestCounter, 1)
	return fmt.Sprintf("guest-%d", id)
}
