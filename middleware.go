package main

import (
	"crypto/ecdsa"
	cryptorand "crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/binary"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"math/rand"
	"net"
	"os"
	"sync"
	"time"

	"github.com/iwanhae/ssh-chat/chat"
	pb "github.com/iwanhae/ssh-chat/proto"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/keepalive"
	"google.golang.org/grpc/peer"
	"google.golang.org/grpc/status"
)

func loadClientPublicKey(path string) (*ecdsa.PublicKey, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read client public key: %w", err)
	}
	block, _ := pem.Decode(data)
	if block == nil {
		return nil, errors.New("client public key is not PEM")
	}
	key, err := x509.ParsePKIXPublicKey(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("parse client public key: %w", err)
	}
	publicKey, ok := key.(*ecdsa.PublicKey)
	if !ok {
		return nil, errors.New("client public key must use ECDSA")
	}
	return publicKey, nil
}

func verifySignature(publicKey *ecdsa.PublicKey, ip string, timestamp int64, signature []byte) error {
	now := time.Now().UnixMilli()
	if timestamp < now-60_000 || timestamp > now+60_000 {
		return errors.New("authentication timestamp is outside the one-minute window")
	}
	message := make([]byte, len(ip)+8)
	copy(message, ip)
	binary.BigEndian.PutUint64(message[len(ip):], uint64(timestamp))
	digest := sha256.Sum256(message)
	if !ecdsa.VerifyASN1(publicKey, digest[:], signature) {
		return errors.New("authentication signature is invalid")
	}
	return nil
}

type gRPCServer struct {
	pb.UnimplementedStreamMiddlewareServer
	chatServer     *chat.ChatServer
	activeStreamMu sync.Mutex
	activeStreams  map[string]chan *pb.ChatMessage
	clientPubKey   *ecdsa.PublicKey
	authEnabled    bool
}

func newGRPCServer(server *chat.ChatServer, authEnabled bool) (*gRPCServer, error) {
	service := &gRPCServer{
		chatServer:    server,
		activeStreams: make(map[string]chan *pb.ChatMessage),
		authEnabled:   authEnabled,
	}
	if authEnabled {
		key, err := loadClientPublicKey("ai_grpc_client.pub")
		if err != nil {
			return nil, err
		}
		service.clientPubKey = key
	}
	return service, nil
}

func (s *gRPCServer) authenticate(message *pb.AIResponse, clientIP string) error {
	if message.Ip == "" || message.AuthTimestamp == 0 || len(message.AuthSignature) == 0 {
		return status.Error(codes.Unauthenticated, "IP, timestamp, and signature are required")
	}
	ip := net.ParseIP(clientIP)
	if (ip == nil || !ip.IsLoopback()) && message.Ip != clientIP {
		return status.Error(codes.Unauthenticated, "signed IP does not match the peer")
	}
	if err := verifySignature(s.clientPubKey, message.Ip, message.AuthTimestamp, message.AuthSignature); err != nil {
		return status.Error(codes.Unauthenticated, err.Error())
	}
	return nil
}

// StreamChat owns its receive goroutine until the RPC context is canceled.
// A stream enters the broadcast map only after its required authentication.
func (s *gRPCServer) StreamChat(stream pb.StreamMiddleware_StreamChatServer) error {
	remote, ok := peer.FromContext(stream.Context())
	if !ok {
		return status.Error(codes.Unauthenticated, "missing peer address")
	}
	clientIP := remote.Addr.String()
	if host, _, err := net.SplitHostPort(clientIP); err == nil {
		clientIP = host
	}
	if s.authEnabled {
		message, err := stream.Recv()
		if err != nil {
			return err
		}
		if err := s.authenticate(message, clientIP); err != nil {
			return err
		}
		if err := s.handleResponse(message); err != nil {
			return err
		}
	}
	id := cryptorand.Text()
	messages := make(chan *pb.ChatMessage, 100)
	s.activeStreamMu.Lock()
	s.activeStreams[id] = messages
	s.activeStreamMu.Unlock()
	defer func() {
		s.activeStreamMu.Lock()
		delete(s.activeStreams, id)
		s.activeStreamMu.Unlock()
	}()

	received := make(chan error, 1)
	go func() {
		for {
			message, err := stream.Recv()
			if errors.Is(err, io.EOF) {
				received <- nil
				return
			}
			if err != nil {
				received <- err
				return
			}
			if err := s.handleResponse(message); err != nil {
				received <- err
				return
			}
		}
	}()
	for {
		select {
		case message := <-messages:
			if err := stream.Send(message); err != nil {
				return err
			}
		case err := <-received:
			return err
		case <-stream.Context().Done():
			return stream.Context().Err()
		}
	}
}

func (s *gRPCServer) handleResponse(message *pb.AIResponse) error {
	if message.MessageId == "" || message.Text == "" {
		return nil
	}
	updated, err := s.chatServer.UpdateMessage(message.MessageId, message.Text)
	if err != nil {
		return err
	}
	if updated {
		return nil
	}
	nick := sanitizeNickname(message.Nick)
	if nick == "" {
		nick = "AI"
	}
	_, err = s.chatServer.AppendMessageWithID(chat.Message{
		ID:    message.MessageId,
		Time:  time.Now(),
		Nick:  nick,
		Text:  message.Text,
		Color: 31 + rand.Intn(6),
	})
	return err
}

func (s *gRPCServer) BroadcastMessage(message chat.Message) {
	if message.Nick == "AI" || message.Nick == "server" {
		return
	}
	out := &pb.ChatMessage{
		MessageId: message.ID,
		Nick:      message.Nick,
		Text:      message.Text,
		Timestamp: message.Time.Unix(),
		Ip:        message.IP,
	}
	s.activeStreamMu.Lock()
	defer s.activeStreamMu.Unlock()
	for _, stream := range s.activeStreams {
		select {
		case stream <- out:
		default:
		}
	}
}

func startGRPCServer(server *chat.ChatServer, securityMode string, authEnabled bool, hostKeyPath string) (*grpc.Server, *gRPCServer, error) {
	service, err := newGRPCServer(server, authEnabled)
	if err != nil {
		return nil, nil, err
	}
	options := []grpc.ServerOption{
		grpc.KeepaliveEnforcementPolicy(keepalive.EnforcementPolicy{
			MinTime:             5 * time.Second,
			PermitWithoutStream: true,
		}),
		grpc.KeepaliveParams(keepalive.ServerParameters{
			MaxConnectionIdle: 9999 * time.Minute,
			Time:              30 * time.Second,
			Timeout:           10 * time.Second,
		}),
	}
	switch securityMode {
	case "none":
	case "tls", "mtls":
		certificate, err := tls.LoadX509KeyPair("grpc_server.cert", hostKeyPath)
		if err != nil {
			return nil, nil, fmt.Errorf("load gRPC certificate: %w", err)
		}
		config := &tls.Config{Certificates: []tls.Certificate{certificate}, MinVersion: tls.VersionTLS12}
		if securityMode == "mtls" {
			data, err := os.ReadFile("ai_grpc_client.ca.cert")
			if err != nil {
				return nil, nil, fmt.Errorf("read gRPC client CA: %w", err)
			}
			config.ClientCAs = x509.NewCertPool()
			if !config.ClientCAs.AppendCertsFromPEM(data) {
				return nil, nil, errors.New("gRPC client CA contains no certificate")
			}
			config.ClientAuth = tls.RequireAndVerifyClientCert
		}
		options = append(options, grpc.Creds(credentials.NewTLS(config)))
	default:
		return nil, nil, fmt.Errorf("invalid gRPC security mode %q; expected none, tls, or mtls", securityMode)
	}
	grpcServer := grpc.NewServer(options...)
	pb.RegisterStreamMiddlewareServer(grpcServer, service)
	return grpcServer, service, nil
}

func stopGRPCServer(server *grpc.Server) {
	done := make(chan struct{})
	go func() {
		server.GracefulStop()
		close(done)
	}()
	timer := time.NewTimer(3 * time.Second)
	defer timer.Stop()
	select {
	case <-done:
	case <-timer.C:
		server.Stop()
		<-done
	}
}
