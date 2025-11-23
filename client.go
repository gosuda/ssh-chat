package main

import (
	"bufio"
	"errors"
	"fmt"
	"log"
	"math/rand"
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
	dbPath            string // SQLite 데이터베이스 파일 경로
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
	rootCmd.Flags().StringVar(&dbPath, "db-path", "chat.db", "path to SQLite database file") // dbPath 플래그 추가
	rootCmd.Flags().Bool("sqlite", false, "enable SQLite message store")

	if err := rootCmd.Execute(); err != nil {
		os.Exit(1)
	}
}

func runServe(cmd *cobra.Command, args []string) error {
	quitCh := make(chan os.Signal, 1)
	signal.Notify(quitCh, os.Interrupt, syscall.SIGTERM, syscall.SIGINT)

	var store chat.MessageStore
	useSqlite, _ := cmd.Flags().GetBool("sqlite")
	if useSqlite {
		s, err := chat.NewSQLiteMessageStore(dbPath)
		if err != nil {
			return fmt.Errorf("failed to create sqlite store: %w", err)
		}
		store = s
	} else {
		store = chat.NewNullMessageStore()
	}

	globalChat, err := chat.NewChatServer(store) // dbPath 전달 및 에러 처리
	if err != nil {
		return fmt.Errorf("채팅 서버 초기화 실패: %w", err)
	}
	defer globalChat.Close() // 서버 종료 시 데이터베이스 연결 닫기

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

		if globalChat.Bans.IsBanned(ip) {
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

		var colors = []int{
			31, 32, 33, 34, 35, 36,
		}
		client := chat.NewClient(globalChat, globalChat.Bans, globalChat.Store, s, finalNickname, int(ptyReq.Window.Width), int(ptyReq.Window.Height), colors[rand.Intn(len(colors))], ip)
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

	return startAndMonitorServer(addr, hostKeyPath, h, globalChat, shutdownCountdown, quitCh)
}

// startAndMonitorServer는 SSH 서버를 시작하고 비정상 종료 시 재시작 로직을 처리합니다.
func startAndMonitorServer(
	addr string,
	hostKeyPath string,
	h ssh.Handler,
	globalChat *chat.ChatServer,
	shutdownCountdown time.Duration,
	quitCh chan os.Signal,
) error {
	// 서버 자동 재시작 루프
	for {
		// 서버 생성
		srv := &ssh.Server{
			Addr:    addr,
			Handler: h,
		}

		if err := srv.SetOption(ssh.HostKeyFile(hostKeyPath)); err != nil {
			return fmt.Errorf("failed to load host key: %w", err)
		}

		errCh := make(chan error, 1)
		go func() {
			// 패닉 발생 시 복구 및 에러 채널로 전송
			defer func() {
				if r := recover(); r != nil {
					err := fmt.Errorf("panic in ListenAndServe goroutine: %v", r)
					log.Printf("💥💥💥💥💥💥또죽었어요. 아파요💥💥💥💥: %v", err) // 패닉 알림
					errCh <- err                                // 외부 루프에 패닉 발생 알림
				}
			}()
			log.Printf("starting ssh chat server on %s ...", addr)
			if err := srv.ListenAndServe(); err != nil && !errors.Is(err, net.ErrClosed) {
				errCh <- err
			}
		}()

		select {
		case sig := <-quitCh:
			log.Printf("received signal: %v", sig)
			runShutdownSequence(globalChat, shutdownCountdown)
			_ = srv.Close() // 새 연결 막고 종료
			return nil
		case err := <-errCh:
			log.Printf("💥💥💥💥💥💥또죽었어요. 아파요💥💥💥💥: %v", err)            // 서버 죽음 알림
			globalChat.AppendSystemMessage("💥💥💥💥💥💥또죽었어요. 아파요💥💥💥💥") // 클라이언트에게도 알림
			_ = srv.Close()                                        // 현재 서버 인스턴스 종료
			// 루프의 다음 반복에서 새 서버 인스턴스가 생성됩니다.
		}
	}
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
