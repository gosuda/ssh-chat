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
	"strings"
	"sync"
	"time"
	"unicode"

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

func authenticationDigest(ip string, timestamp int64) [32]byte {
	message := make([]byte, len(ip)+8)
	copy(message, ip)
	binary.BigEndian.PutUint64(message[len(ip):], uint64(timestamp))
	return sha256.Sum256(message)
}

func verifySignature(publicKey *ecdsa.PublicKey, ip string, timestamp int64, signature []byte) error {
	now := time.Now().UnixMilli()
	if timestamp < now-60_000 || timestamp > now+60_000 {
		return errors.New("authentication timestamp is outside the one-minute window")
	}
	digest := authenticationDigest(ip, timestamp)
	if !ecdsa.VerifyASN1(publicKey, digest[:], signature) {
		return errors.New("authentication signature is invalid")
	}
	return nil
}

type middlewareStream struct {
	messages chan *pb.ChatMessage
	overflow chan struct{}
}

type replyState struct {
	id   string
	text string
}

type gRPCServer struct {
	pb.UnimplementedStreamMiddlewareServer
	chatServer          *chat.ChatServer
	activeStreamMu      sync.Mutex
	activeStreams       map[string]*middlewareStream
	authMu              sync.Mutex
	usedAuthentications map[[32]byte]int64
	clientPubKey        *ecdsa.PublicKey
	authEnabled         bool
}

func newGRPCServer(server *chat.ChatServer, authEnabled bool) (*gRPCServer, error) {
	service := &gRPCServer{
		chatServer:          server,
		activeStreams:       make(map[string]*middlewareStream),
		usedAuthentications: make(map[[32]byte]int64),
		authEnabled:         authEnabled,
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
	digest := authenticationDigest(message.Ip, message.AuthTimestamp)
	s.authMu.Lock()
	defer s.authMu.Unlock()
	now := time.Now().UnixMilli()
	for token, expires := range s.usedAuthentications {
		if expires < now {
			delete(s.usedAuthentications, token)
		}
	}
	if _, exists := s.usedAuthentications[digest]; exists {
		return status.Error(codes.Unauthenticated, "authentication has already been used")
	}
	if len(s.usedAuthentications) >= 4096 {
		return status.Error(codes.ResourceExhausted, "authentication window is full")
	}
	s.usedAuthentications[digest] = message.AuthTimestamp + 60_000
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
	replies := make(map[string]replyState)
	if s.authEnabled {
		message, err := stream.Recv()
		if err != nil {
			return err
		}
		if err := s.authenticate(message, clientIP); err != nil {
			return err
		}
		if err := s.handleResponse(message, replies); err != nil {
			return err
		}
	}
	id := cryptorand.Text()
	subscription := &middlewareStream{messages: make(chan *pb.ChatMessage, 100), overflow: make(chan struct{})}
	s.activeStreamMu.Lock()
	s.activeStreams[id] = subscription
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
			if err := s.handleResponse(message, replies); err != nil {
				received <- err
				return
			}
		}
	}()
	for {
		select {
		case <-subscription.overflow:
			return status.Error(codes.ResourceExhausted, "middleware feed exceeded its queue; reconnect")
		case message := <-subscription.messages:
			if err := stream.Send(message); err != nil {
				return err
			}
		case err := <-received:
			if err != nil {
				return err
			}
			// A client may stop sending replies while it continues receiving chat.
			received = nil
		case <-stream.Context().Done():
			return stream.Context().Err()
		}
	}
}

// Each RPC owns its reply map. Prompt IDs select replies within that RPC;
// generated storage IDs keep prompts and independent assistants distinct.
func (s *gRPCServer) handleResponse(message *pb.AIResponse, replies map[string]replyState) error {
	switch message.Format {
	case pb.ResponseFormat_SNAPSHOT, pb.ResponseFormat_DELTA:
	default:
		return status.Error(codes.InvalidArgument, "unknown response format")
	}
	if message.MessageId == "" {
		return nil
	}
	reply, exists := replies[message.MessageId]
	if message.Text == "" {
		if message.IsFinal {
			delete(replies, message.MessageId)
			return nil
		}
		if !exists || message.Format == pb.ResponseFormat_DELTA {
			return nil
		}
	}
	if !exists && !message.IsFinal && len(replies) >= 100 {
		return status.Error(codes.ResourceExhausted, "too many unfinished replies")
	}
	text := sanitizeReplyText(message.Text)
	if message.Format == pb.ResponseFormat_DELTA {
		if len(reply.text)+len(text) > 64*1024 {
			return status.Error(codes.ResourceExhausted, "reply exceeds 64 KiB")
		}
		text = reply.text + text
	}
	if len(text) > 64*1024 {
		return status.Error(codes.ResourceExhausted, "reply exceeds 64 KiB")
	}
	if exists {
		updated, err := s.chatServer.UpdateMessage(reply.id, text)
		if err != nil {
			return err
		}
		if !updated {
			return status.Error(codes.NotFound, "reply is no longer in chat history")
		}
	} else {
		nick := sanitizeNickname(message.Nick)
		if nick == "" {
			nick = "AI"
		}
		stored, err := s.chatServer.AppendMessageWithID(chat.Message{
			Time: time.Now(), Nick: nick, Text: text, Color: 31 + rand.Intn(6),
		})
		if err != nil {
			return err
		}
		reply.id = stored.ID
	}
	reply.text = text
	if message.IsFinal {
		delete(replies, message.MessageId)
	} else {
		replies[message.MessageId] = reply
	}
	return nil
}

func sanitizeReplyText(raw string) string {
	var text strings.Builder
	text.Grow(len(raw))
	for _, r := range raw {
		switch {
		case unicode.IsGraphic(r) || r == '\u200c' || r == '\u200d':
			text.WriteRune(r)
		case unicode.IsSpace(r):
			text.WriteByte(' ')
		}
	}
	return text.String()
}

func (s *gRPCServer) BroadcastMessage(message chat.Message) {
	out := &pb.ChatMessage{
		MessageId: message.ID,
		Nick:      message.Nick,
		Text:      message.Text,
		Timestamp: message.Time.Unix(),
		Ip:        message.IP,
	}
	s.activeStreamMu.Lock()
	defer s.activeStreamMu.Unlock()
	for id, stream := range s.activeStreams {
		select {
		case stream.messages <- out:
		default:
			close(stream.overflow)
			delete(s.activeStreams, id)
		}
	}
}

func startGRPCServer(server *chat.ChatServer, securityMode string, authEnabled bool, hostKeyPath string) (*grpc.Server, *gRPCServer, error) {
	if authEnabled && securityMode == "none" {
		return nil, nil, errors.New("signed middleware authentication requires TLS")
	}
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
