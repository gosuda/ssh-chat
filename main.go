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
	"sync/atomic"
	"syscall"
	"time"

	"github.com/gliderlabs/ssh"
	"github.com/iwanhae/ssh-chat/chat"
	"github.com/spf13/cobra"
)

var (
	addr              string
	hostKeyPath       string
	maxPerIP          int
	shutdownCountdown time.Duration
	guestCounter      uint64
)

func main() {
	rootCmd := &cobra.Command{
		Use:   "ssh-chat",
		Short: "A tiny SSH chat server",
		Long:  "A tiny SSH chat server using gliderlabs/ssh and a libp2p-friendly chat core.",
		RunE:  runServe,
	}

	rootCmd.Flags().StringVar(&addr, "addr", ":2222", "listen address (e.g. :2222 or 0.0.0.0:2222)")
	rootCmd.Flags().StringVar(&hostKeyPath, "host-key", "host.key", "path to SSH host private key")
	rootCmd.Flags().IntVar(&maxPerIP, "max-per-ip", 2, "max simultaneous connections allowed per IP")
	rootCmd.Flags().DurationVar(&shutdownCountdown, "shutdown-countdown", 5*time.Second, "graceful shutdown countdown duration")

	if err := rootCmd.Execute(); err != nil {
		os.Exit(1)
	}
}

func runServe(cmd *cobra.Command, args []string) error {
	quitCh := make(chan os.Signal, 1)
	signal.Notify(quitCh, os.Interrupt, syscall.SIGTERM, syscall.SIGINT)

	banManager := chat.NewBanManager()
	globalChat := chat.NewChatServer()

	// SSH 세션 핸들러
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

		if !globalChat.CheckIPLimit(ip) {
			fmt.Fprintln(s, "Connection limit exceeded for this IP.")
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

		finalNickname := globalChat.GetUniqueNickname(nickname)

		client := chat.NewClient(globalChat, s, finalNickname, int(ptyReq.Window.Width), int(ptyReq.Window.Height), ip)
		globalChat.AddClient(client)
		defer func() {
			globalChat.RemoveClient(client)
			client.Close()
			globalChat.AppendSystemMessage(fmt.Sprintf("%s left the chat", finalNickname))
		}()

		// 화면 초기화 & 입장 알림
		fmt.Fprint(s, "\x1b[2J\x1b[H")
		globalChat.AppendSystemMessage(fmt.Sprintf("%s joined the chat", finalNickname))

		// 창 사이즈 모니터링 + 메시지 루프
		go client.MonitorWindow(winCh)
		client.Start(reader, s.Context())
		client.Wait()
	}

	// 서버 생성
	srv := &ssh.Server{
		Addr:    addr,
		Handler: h,
	}

	if err := srv.SetOption(ssh.HostKeyFile(hostKeyPath)); err != nil {
		return fmt.Errorf("failed to load host key: %w", err)
	}

	// 서버 실행
	errCh := make(chan error, 1)
	go func() {
		log.Printf("starting ssh chat server on %s ...", addr)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, net.ErrClosed) {
			errCh <- err
		}
	}()

	// 종료 대기
	select {
	case sig := <-quitCh:
		log.Printf("received signal: %v", sig)
	case err := <-errCh:
		log.Printf("ssh server error: %v", err)
	}

	runShutdownSequence(globalChat, shutdownCountdown)

	// 새 연결 막고 종료
	_ = srv.Close()
	return nil
}

func runShutdownSequence(globalChat *chat.ChatServer, countdown time.Duration) {
	if countdown <= 0 {
		return
	}
	sec := int(countdown.Seconds())
	globalChat.AppendSystemMessage(fmt.Sprintf("서버 폭파 %d초 전", sec))
	for i := sec; i >= 0; i-- {
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
}

func generateGuestNickname() string {
	id := atomic.AddUint64(&guestCounter, 1)
	return fmt.Sprintf("guest-%d", id)
}
