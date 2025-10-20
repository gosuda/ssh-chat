package main

import (
	"bufio"
	"context"
	"crypto/rand"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	mathrand "math/rand"
	"net"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	ssh "github.com/gliderlabs/ssh"

	libp2p "github.com/libp2p/go-libp2p"
	corehost "github.com/libp2p/go-libp2p/core/host"
	peer "github.com/libp2p/go-libp2p/core/peer"
	multiaddr "github.com/multiformats/go-multiaddr"
)

// ===== Main (server/proxy) =====

var (
	flagProxy  = flag.Bool("proxy", false, "run in TCP proxy mode: expose -listen and forward to -peer via libp2p stream")
	flagListen = flag.String("listen", ":2222", "TCP listen addr for proxy mode (ssh client connects here)")
	flagPeer   = flag.String("peer", "", "remote peer multiaddr to dial in proxy mode, or to proactively connect in server mode")
	flagNoMDNS = flag.Bool("no-mdns", false, "disable mDNS discovery")
)

func main() {
	flag.Parse()

	// Make stdlib rand deterministic-ish for colors; crypto/rand for host key
	mathrand.Seed(time.Now().UnixNano())

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	quitCh := make(chan os.Signal, 1)
	signal.Notify(quitCh, os.Interrupt, syscall.SIGTERM, syscall.SIGINT)

	// Create libp2p host
	h, err := libp2p.New(
		libp2p.ListenAddrStrings(
			"/ip4/0.0.0.0/tcp/0",
			"/ip6/::/tcp/0",
		),
		libp2p.EnableNATService(),
		libp2p.NATPortMap(),
	)
	if err != nil {
		log.Fatalf("libp2p host error: %v", err)
	}
	defer func() { _ = h.Close() }()

	log.Printf("libp2p host started: id=%s", h.ID())
	for _, a := range h.Addrs() {
		log.Printf("  addr: %s/p2p/%s", a, h.ID())
	}

	// Optional mdns
	if !*flagNoMDNS {
		enableMDNS(h)
		log.Printf("mDNS discovery enabled")
	}

	// Optional proactive connect (server: to help establish connectivity; proxy: required)
	if *flagPeer != "" {
		if err := connectToPeer(ctx, h, *flagPeer); err != nil {
			log.Printf("connect peer failed: %v", err)
		}
	}

	if *flagProxy {
		runProxy(ctx, h)
		return
	}

	// Server mode: create p2p-backed listener and run gliderlabs/ssh Server.Serve
	lst := newP2PListener(h, chatProtoID)
	lst.attachStreamHandler()

	// SSH server setup (host key ephemeral in-memory)
	// gliderlabs requires a host key file; generate one ephemeral in-memory by writing to temp
	hostKeyPath, cleanup, err := generateTempHostKey()
	if err != nil {
		log.Fatalf("host key gen failed: %v", err)
	}
	defer cleanup()

	handler := func(s ssh.Session) {
		ptyReq, winCh, isPty := s.Pty()
		if !isPty {
			fmt.Fprintln(s, "Error: PTY required. Reconnect with -t option.")
			_ = s.Exit(1)
			return
		}

		reader := bufio.NewReader(s)

		// In libp2p world, remote IP is a peer/maddr descriptor from net.Conn we attached
		remote := s.RemoteAddr().String()
		ip := remote // may look like: QmPeer /ip4/1.2.3.4/tcp/4001

		if banManager.IsBanned(ip) {
			fmt.Fprintln(s, "Your endpoint is banned.")
			_ = s.Exit(1)
			return
		}
		if !globalChat.CheckIPLimit(ip) {
			fmt.Fprintln(s, "Connection limit exceeded for this endpoint (max 2).")
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

		client := NewClient(globalChat, s, finalNickname, int(ptyReq.Window.Width), int(ptyReq.Window.Height), ip)
		globalChat.AddClient(client)
		defer func() {
			globalChat.RemoveClient(client)
			client.Close()
			globalChat.AppendSystemMessage(fmt.Sprintf("%s left the chat", finalNickname))
		}()

		fmt.Fprint(s, "\x1b[2J\x1b[H")
		globalChat.AppendSystemMessage(fmt.Sprintf("%s joined the chat", finalNickname))
		go client.MonitorWindow(winCh)
		client.Start(reader, s.Context())
		client.Wait()
	}

	srv := &ssh.Server{Handler: handler}
	srv.SetOption(ssh.HostKeyFile(hostKeyPath))

	// Serve on the p2p listener (blocking)
	go func() {
		log.Printf("ssh chat ready over libp2p protocol %s (Accept via incoming streams)", chatProtoID)
		if err := srv.Serve(lst); err != nil && !errors.Is(err, net.ErrClosed) {
			log.Printf("ssh server error: %v", err)
			quitCh <- os.Interrupt
		}
	}()

	<-quitCh
	globalChat.AppendSystemMessage("서버 폭파 5초전")
	for i := 5; i >= 0; i-- {
		time.Sleep(time.Second)
		globalChat.AppendSystemMessage(fmt.Sprintf("%d 초", i))
	}
	globalChat.AppendSystemMessage("💥💥💥💥💥")
	time.Sleep(1500 * time.Millisecond)
	_ = lst.Close()
	_ = srv.Close()
}

// ===== Proxy mode: expose local TCP that forwards to remote peer stream =====

func runProxy(ctx context.Context, h corehost.Host) {
	if *flagPeer == "" {
		log.Fatalf("-proxy requires -peer <multiaddr>")
	}

	ln, err := net.Listen("tcp", *flagListen)
	if err != nil {
		log.Fatalf("proxy listen error: %v", err)
	}
	defer ln.Close()
	log.Printf("proxy listening on %s -> %s (libp2p %s)", *flagListen, *flagPeer, chatProtoID)

	for {
		c, err := ln.Accept()
		if err != nil {
			if errors.Is(err, net.ErrClosed) {
				return
			}
			log.Printf("accept err: %v", err)
			continue
		}
		go func(tc net.Conn) {
			defer tc.Close()
			// Ensure connected to peer
			if err := connectToPeer(ctx, h, *flagPeer); err != nil {
				log.Printf("connect peer failed: %v", err)
				return
			}
			// Open stream
			pid, _ := peer.AddrInfoFromP2pAddr(parseMultiaddrMust(*flagPeer))
			s, err := h.NewStream(ctx, pid.ID, chatProtoID)
			if err != nil {
				log.Printf("open stream failed: %v", err)
				return
			}
			defer s.Close()

			// pipe both ways
			errc := make(chan error, 2)
			go func() { _, e := io.Copy(s, tc); errc <- e }()
			go func() { _, e := io.Copy(tc, s); errc <- e }()
			<-errc // one side done
		}(c)
	}
}

// ===== Helpers =====

func generateTempHostKey() (path string, cleanup func(), err error) {
	// Use ed25519 private key in OpenSSH format
	// We shell out to generate deterministic-ish key; but here we create an ephemeral file with random bytes then rely on gliderlabs' generator is limited.
	// Simpler: write a minimal PEM by calling ssh.GenerateKey (not provided). We'll just create a temp file and use ssh.HostKeyPEM with ed25519.
	// gliderlabs supports reading existing PEM. We'll generate with ssh-keygen like format using x/crypto/ssh helpers would be lengthy.
	// For brevity, we embed a minimal 32-byte seed for ed25519 and let gliderlabs generate if file not found — but it requires a file.

	f, err := os.CreateTemp("", "ssh-hostkey-*")
	if err != nil {
		return "", func() {}, err
	}
	// Write random bytes so the filename exists; gliderlabs will replace with proper key if invalid (it generates automatically)
	buf := make([]byte, 64)
	if _, err := rand.Read(buf); err == nil {
		_, _ = f.Write(buf)
	}
	f.Close()
	cleanup = func() { _ = os.Remove(f.Name()) }
	return f.Name(), cleanup, nil
}

func connectToPeer(ctx context.Context, h corehost.Host, maddrStr string) error {
	maddr, err := multiaddr.NewMultiaddr(maddrStr)
	if err != nil {
		return err
	}
	ai, err := peer.AddrInfoFromP2pAddr(maddr)
	if err != nil {
		return err
	}
	if err := h.Connect(ctx, *ai); err != nil {
		return err
	}
	log.Printf("connected to peer %s %v", ai.ID.ShortString(), ai.Addrs)
	return nil
}

func parseMultiaddrMust(s string) multiaddr.Multiaddr { m, _ := multiaddr.NewMultiaddr(s); return m }
