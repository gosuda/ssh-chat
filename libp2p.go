package main

import (
	"errors"
	"fmt"
	"log"
	"net"
	"sync"
	"time"

	corehost "github.com/libp2p/go-libp2p/core/host"
	network "github.com/libp2p/go-libp2p/core/network"
	peer "github.com/libp2p/go-libp2p/core/peer"
	"github.com/libp2p/go-libp2p/p2p/discovery/mdns"
)

// --------------------------------
//  libp2p <-> net.Listener bridge
// --------------------------------

const chatProtoID = "/ssh-chat/1.0.0"

// streamConn wraps a libp2p stream to satisfy net.Conn
type streamConn struct {
	s            network.Stream
	laddr, raddr net.Addr
}

func (c *streamConn) Read(p []byte) (int, error)  { return c.s.Read(p) }
func (c *streamConn) Write(p []byte) (int, error) { return c.s.Write(p) }
func (c *streamConn) Close() error                { return c.s.Close() }

func (c *streamConn) LocalAddr() net.Addr  { return c.laddr }
func (c *streamConn) RemoteAddr() net.Addr { return c.raddr }

// Prefer delegating deadlines to the underlying libp2p stream.
// If your libp2p version lacks SetDeadline, you can emulate by calling both.
func (c *streamConn) SetDeadline(t time.Time) error {
	// best-effort: set both; return the first error if any
	if err := c.s.SetReadDeadline(t); err != nil {
		return err
	}
	return c.s.SetWriteDeadline(t)
}

func (c *streamConn) SetReadDeadline(t time.Time) error  { return c.s.SetReadDeadline(t) }
func (c *streamConn) SetWriteDeadline(t time.Time) error { return c.s.SetWriteDeadline(t) }

// p2pAddr is a lightweight net.Addr that prints peer + multiaddr
type p2pAddr struct {
	network string
	desc    string
}

func (a p2pAddr) Network() string { return a.network }
func (a p2pAddr) String() string  { return a.desc }

// p2pListener collects incoming libp2p streams and exposes a net.Listener API
type p2pListener struct {
	host  corehost.Host
	proto string

	mu     sync.Mutex
	ch     chan net.Conn
	closed bool
}

func newP2PListener(h corehost.Host, proto string) *p2pListener {
	return &p2pListener{host: h, proto: proto, ch: make(chan net.Conn, 64)}
}

func (l *p2pListener) Accept() (net.Conn, error) {
	c, ok := <-l.ch
	if !ok {
		return nil, errors.New("listener closed")
	}
	return c, nil
}

func (l *p2pListener) Close() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.closed {
		return nil
	}
	l.closed = true
	close(l.ch)
	return nil
}

func (l *p2pListener) Addr() net.Addr {
	return p2pAddr{network: "libp2p", desc: l.host.ID().String()}
}

// attachStreamHandler wires libp2p streams into the listener.Accept queue
func (l *p2pListener) attachStreamHandler() {
	l.host.SetStreamHandler(chatProtoID, func(s network.Stream) {
		// Build nice addr strings
		rpid := s.Conn().RemotePeer()
		rma := s.Conn().RemoteMultiaddr()
		lma := s.Conn().LocalMultiaddr()
		laddr := p2pAddr{network: "libp2p", desc: fmt.Sprintf("%s %s", l.host.ID().ShortString(), lma)}
		raddr := p2pAddr{network: "libp2p", desc: fmt.Sprintf("%s %s", peer.ID(rpid).ShortString(), rma)}

		conn := &streamConn{s: s, laddr: laddr, raddr: raddr}
		// queue for Accept(); if listener closed, reset stream
		select {
		case l.ch <- conn:
		default:
			_ = s.Reset()
		}
	})
}

// mdns service (discover peers on LAN)

type mdnsNotifee struct{ h corehost.Host }

func (m *mdnsNotifee) HandlePeerFound(pi peer.AddrInfo) {
	log.Printf("mDNS found peer: %s addrs=%v", pi.ID.ShortString(), pi.Addrs)
}

func enableMDNS(h corehost.Host) (stop func()) {
	svc := mdns.NewMdnsService(h, "ssh-chat-mdns", &mdnsNotifee{h: h})
	if starter, ok := any(svc).(interface{ Start() error }); ok {
		_ = starter.Start()
	}
	return func() {
		if closer, ok := any(svc).(interface{ Close() error }); ok {
			_ = closer.Close()
		}
	}
}
