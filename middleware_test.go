package main

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/binary"
	"encoding/pem"
	"fmt"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/iwanhae/ssh-chat/chat"
	pb "github.com/iwanhae/ssh-chat/proto"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
)

func await(t *testing.T, condition func() bool) {
	t.Helper()
	deadline := time.NewTimer(5 * time.Second)
	defer deadline.Stop()
	tick := time.NewTicker(time.Millisecond)
	defer tick.Stop()
	for !condition() {
		select {
		case <-deadline.C:
			t.Fatal("condition did not become true within five seconds")
		case <-tick.C:
		}
	}
}

func streamCount(service *gRPCServer) int {
	service.activeStreamMu.Lock()
	defer service.activeStreamMu.Unlock()
	return len(service.activeStreams)
}

func startTestRPC(t *testing.T, server *grpc.Server, service *gRPCServer, transport credentials.TransportCredentials) pb.StreamMiddlewareClient {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	stopped := make(chan error, 1)
	go func() { stopped <- server.Serve(listener) }()
	t.Cleanup(func() {
		server.Stop()
		select {
		case err := <-stopped:
			if err != nil {
				t.Error(err)
			}
		case <-time.After(5 * time.Second):
			t.Error("gRPC server did not stop")
		}
	})
	connection, err := grpc.NewClient(listener.Addr().String(), grpc.WithTransportCredentials(transport))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := connection.Close(); err != nil {
			t.Error(err)
		}
	})
	return pb.NewStreamMiddlewareClient(connection)
}

func TestBidirectionalMessagesSurviveTheStorageRefactor(t *testing.T) {
	for _, kind := range []string{"memory", "sqlite"} {
		t.Run(kind, func(t *testing.T) {
			var store chat.MessageStore = chat.NewMemoryMessageStore()
			if kind == "sqlite" {
				var err error
				store, err = chat.NewSQLiteMessageStore(filepath.Join(t.TempDir(), "chat.db"))
				if err != nil {
					t.Fatal(err)
				}
			}
			core, err := chat.NewChatServer(store)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				if err := core.Close(); err != nil {
					t.Error(err)
				}
			})
			server, service, err := startGRPCServer(core, "none", false, "unused")
			if err != nil {
				t.Fatal(err)
			}
			core.SetUserMessageHandler(service.BroadcastMessage)
			client := startTestRPC(t, server, service, insecure.NewCredentials())
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			stream, err := client.StreamChat(ctx)
			if err != nil {
				t.Fatal(err)
			}
			await(t, func() bool { return streamCount(service) == 1 })
			core.AppendUserMessage(chat.Message{Time: time.Now(), Nick: "reader", Text: "question", IP: "192.0.2.1", Color: 32})
			first, err := stream.Recv()
			if err != nil {
				t.Fatal(err)
			}
			if first.MessageId == "" || first.Nick != "reader" || first.Text != "question" || first.Ip != "192.0.2.1" {
				t.Fatalf("incorrect outbound message: %v", first)
			}
			if err := stream.Send(&pb.AIResponse{MessageId: first.MessageId, Nick: "Assistant", Text: "part\x1b", Format: pb.ResponseFormat_DELTA}); err != nil {
				t.Fatal(err)
			}
			replyID := ""
			await(t, func() bool {
				for _, m := range core.Messages() {
					if m.ID != first.MessageId && m.Nick == "Assistant" && m.Text == "part" {
						replyID = m.ID
						return true
					}
				}
				return false
			})
			if err := stream.Send(&pb.AIResponse{MessageId: first.MessageId, Text: " complete\nanswer\u009b", IsFinal: true, Format: pb.ResponseFormat_DELTA}); err != nil {
				t.Fatal(err)
			}
			await(t, func() bool {
				for _, m := range core.Messages() {
					if m.ID == replyID && m.Text == "part complete answer" {
						return true
					}
				}
				return false
			})
			prompts, replies := 0, 0
			for _, message := range core.Messages() {
				if message.ID == first.MessageId {
					prompts++
					if message.Nick != "reader" || message.Text != "question" {
						t.Fatalf("prompt was overwritten: %+v", message)
					}
				}
				if message.Nick == "Assistant" {
					replies++
					if message.ID != replyID || message.Text != "part complete answer" {
						t.Fatalf("reply identity or incremental text changed: %+v", message)
					}
				}
			}
			if prompts != 1 || replies != 1 {
				t.Fatalf("prompt and reply must coexist exactly once: prompts=%d replies=%d", prompts, replies)
			}
			core.AppendSystemMessage("administration")
			core.AppendUserMessage(chat.Message{Time: time.Now(), Nick: "reader", Text: "next", Color: 32})
			next, err := stream.Recv()
			if err != nil {
				t.Fatal(err)
			}
			if next.Text != "next" || next.MessageId == first.MessageId {
				t.Fatalf("response loop or unstable ID: %v", next)
			}
			for _, nickname := range []string{"AI", "server"} {
				core.AppendUserMessage(chat.Message{Time: time.Now(), Nick: nickname, Text: "human", Color: 32})
				delivered, err := stream.Recv()
				if err != nil || delivered.GetNick() != nickname {
					t.Fatalf("user nickname was filtered: %q %v %v", nickname, delivered, err)
				}
			}
			if err := stream.CloseSend(); err != nil {
				t.Fatal(err)
			}
			type delivery struct {
				message *pb.ChatMessage
				err     error
			}
			pending := make(chan delivery, 1)
			go func() {
				message, err := stream.Recv()
				pending <- delivery{message: message, err: err}
			}()
			select {
			case early := <-pending:
				t.Fatalf("half-close ended the receive feed: %v", early.err)
			case <-time.After(50 * time.Millisecond):
			}
			core.AppendUserMessage(chat.Message{Time: time.Now(), Nick: "reader", Text: "after half-close", Color: 32})
			select {
			case delivered := <-pending:
				if delivered.err != nil || delivered.message.GetText() != "after half-close" {
					t.Fatalf("half-closed feed did not receive chat: %+v", delivered)
				}
			case <-time.After(5 * time.Second):
				t.Fatal("half-closed feed timed out")
			}
			cancel()
			await(t, func() bool { return streamCount(service) == 0 })
		})
	}
}

func signAuthentication(t *testing.T, key *ecdsa.PrivateKey, ip string, timestamp int64) []byte {
	t.Helper()
	message := make([]byte, len(ip)+8)
	copy(message, ip)
	binary.BigEndian.PutUint64(message[len(ip):], uint64(timestamp))
	digest := sha256.Sum256(message)
	signature, err := ecdsa.SignASN1(rand.Reader, key, digest[:])
	if err != nil {
		t.Fatal(err)
	}
	return signature
}

func TestAuthenticationPrecedesBroadcastRegistration(t *testing.T) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	core, err := chat.NewChatServer(chat.NewMemoryMessageStore())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := core.Close(); err != nil {
			t.Error(err)
		}
	})
	service := &gRPCServer{chatServer: core, activeStreams: make(map[string]*middlewareStream), usedAuthentications: make(map[[32]byte]int64), clientPubKey: &key.PublicKey, authEnabled: true}
	server := grpc.NewServer()
	pb.RegisterStreamMiddlewareServer(server, service)
	client := startTestRPC(t, server, service, insecure.NewCredentials())
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	invalid, err := client.StreamChat(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := invalid.Send(&pb.AIResponse{Ip: "127.0.0.1", AuthTimestamp: time.Now().UnixMilli(), AuthSignature: []byte("invalid")}); err != nil {
		t.Fatal(err)
	}
	if _, err := invalid.Recv(); status.Code(err) != codes.Unauthenticated {
		t.Fatalf("invalid signature accepted: %v", err)
	}
	if streamCount(service) != 0 {
		t.Fatal("unauthenticated stream entered broadcast map")
	}
	valid, err := client.StreamChat(ctx)
	if err != nil {
		t.Fatal(err)
	}
	timestamp := time.Now().UnixMilli()
	signature := signAuthentication(t, key, "127.0.0.1", timestamp)
	if err := valid.Send(&pb.AIResponse{Ip: "127.0.0.1", AuthTimestamp: timestamp, AuthSignature: signature}); err != nil {
		t.Fatal(err)
	}
	await(t, func() bool { return streamCount(service) == 1 })
	duplicate, err := client.StreamChat(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := duplicate.Send(&pb.AIResponse{Ip: "127.0.0.1", AuthTimestamp: timestamp, AuthSignature: signature}); err != nil {
		t.Fatal(err)
	}
	if _, err := duplicate.Recv(); status.Code(err) != codes.Unauthenticated {
		t.Fatalf("replayed authentication accepted: %v", err)
	}
	if streamCount(service) != 1 {
		t.Fatal("replayed stream entered broadcast map")
	}
	service.BroadcastMessage(chat.Message{ID: "authenticated", Time: time.Now(), Nick: "reader", Text: "visible"})
	got, err := valid.Recv()
	if err != nil || got.GetMessageId() != "authenticated" {
		t.Fatalf("authenticated delivery failed: %v %v", got, err)
	}
	old := timestamp - 120_000
	if err := verifySignature(&key.PublicKey, "127.0.0.1", old, signAuthentication(t, key, "127.0.0.1", old)); err == nil {
		t.Fatal("stale signature accepted")
	}
	if err := verifySignature(&key.PublicKey, "192.0.2.1", timestamp, signAuthentication(t, key, "127.0.0.1", timestamp)); err == nil {
		t.Fatal("signature for a different IP accepted")
	}
}

func TestTLSModesAndCustomHostKey(t *testing.T) {
	t.Chdir(t.TempDir())
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	template := &x509.Certificate{
		SerialNumber: big.NewInt(1), NotBefore: time.Now().Add(-time.Minute), NotAfter: time.Now().Add(time.Hour),
		IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature,
		ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth, x509.ExtKeyUsageClientAuth},
		IPAddresses: []net.IP{net.ParseIP("127.0.0.1")},
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	certificatePEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	privateDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile("grpc_server.cert", certificatePEM, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile("custom-host.key", pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: privateDER}), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile("ai_grpc_client.ca.cert", certificatePEM, 0600); err != nil {
		t.Fatal(err)
	}
	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM(certificatePEM) {
		t.Fatal("test certificate is invalid")
	}
	certificate, err := tls.LoadX509KeyPair("grpc_server.cert", "custom-host.key")
	if err != nil {
		t.Fatal(err)
	}
	for _, mode := range []string{"tls", "mtls"} {
		t.Run(mode, func(t *testing.T) {
			core, err := chat.NewChatServer(chat.NewMemoryMessageStore())
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				if err := core.Close(); err != nil {
					t.Error(err)
				}
			})
			server, service, err := startGRPCServer(core, mode, false, "custom-host.key")
			if err != nil {
				t.Fatal(err)
			}
			transport := credentials.NewTLS(&tls.Config{RootCAs: roots, Certificates: []tls.Certificate{certificate}, MinVersion: tls.VersionTLS12})
			client := startTestRPC(t, server, service, transport)
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			stream, err := client.StreamChat(ctx)
			if err != nil {
				t.Fatal(err)
			}
			await(t, func() bool { return streamCount(service) == 1 })
			service.BroadcastMessage(chat.Message{ID: "secure", Time: time.Now(), Nick: "reader", Text: "secured"})
			got, err := stream.Recv()
			if err != nil || got.GetMessageId() != "secure" {
				t.Fatalf("secure delivery failed: %v %v", got, err)
			}
		})
	}
}

func TestCLIContainsBothSetsOfOptions(t *testing.T) {
	command := newRootCommand()
	for _, flag := range []string{"addr", "host-key", "max-per-ip", "shutdown-countdown", "db-path", "sqlite", "ssh-port", "grpc-port", "grpc-host", "grpc-security", "grpc-auth"} {
		if command.Flags().Lookup(flag) == nil {
			t.Fatalf("lost option --%s", flag)
		}
	}
	if grpcPort != "3333" || grpcHost != "127.0.0.1" || sshPort != "2222" || grpcSecurityMode != "none" || grpcAuthEnabled {
		t.Fatal("middleware defaults changed")
	}
	if got := sanitizeNickname("\x1breader\n"); got != "reader" {
		t.Fatalf("nickname sanitizer lost: %q", got)
	}
}

func TestListenerCollisionDoesNotTouchHistory(t *testing.T) {
	for _, flag := range []string{"addr", "ssh-port"} {
		t.Run(flag, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "history.db")
			store, err := chat.NewSQLiteMessageStore(path)
			if err != nil {
				t.Fatal(err)
			}
			if err := store.Init(); err != nil {
				t.Fatal(err)
			}
			if err := store.AppendMessage(chat.Message{ID: "retained", Time: time.Now(), Nick: "reader", Text: "history", Color: 32}); err != nil {
				t.Fatal(err)
			}
			if err := store.Close(); err != nil {
				t.Fatal(err)
			}
			before, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			reservation, err := net.Listen("tcp", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			address := reservation.Addr().String()
			_, port, err := net.SplitHostPort(address)
			if err != nil {
				t.Fatal(err)
			}
			if err := reservation.Close(); err != nil {
				t.Fatal(err)
			}
			command := newRootCommand()
			value := address
			if flag == "ssh-port" {
				value = port
			}
			command.SetArgs([]string{"--" + flag, value, "--grpc-port", port, "--sqlite", "--db-path", path})
			if err := command.Execute(); err == nil || !strings.Contains(err.Error(), "listen for gRPC middleware") {
				t.Fatalf("listener collision did not fail before server startup: %v", err)
			}
			after, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(before, after) {
				t.Fatal("listener collision changed chat history")
			}
			released, err := net.Listen("tcp", address)
			if err != nil {
				t.Fatalf("failed startup leaked SSH listener: %v", err)
			}
			if err := released.Close(); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestRemoteMiddlewareRequiresAuthenticatedTransport(t *testing.T) {
	for _, host := range []string{"0.0.0.0", "::", "192.0.2.1"} {
		for _, mode := range []string{"none", "tls"} {
			if _, err := middlewareAddress(host, "3333", mode, false); err == nil {
				t.Fatalf("unauthenticated remote middleware allowed: %s %s", host, mode)
			}
		}
		if _, err := middlewareAddress(host, "3333", "mtls", false); err != nil {
			t.Fatal(err)
		}
		if _, err := middlewareAddress(host, "3333", "tls", true); err != nil {
			t.Fatal(err)
		}
	}
	if address, err := middlewareAddress("127.0.0.1", "3333", "none", false); err != nil || address != "127.0.0.1:3333" {
		t.Fatalf("loopback default failed: %q %v", address, err)
	}
	if _, err := middlewareAddress("127.0.0.1", "3333", "none", true); err == nil {
		t.Fatal("signed authentication allowed without TLS")
	}
}

func TestBroadcastOverflowClosesTheSubscriber(t *testing.T) {
	subscription := &middlewareStream{messages: make(chan *pb.ChatMessage, 1), overflow: make(chan struct{})}
	service := &gRPCServer{activeStreams: map[string]*middlewareStream{"slow": subscription}}
	service.BroadcastMessage(chat.Message{ID: "first", Nick: "reader", Text: "first"})
	service.BroadcastMessage(chat.Message{ID: "second", Nick: "reader", Text: "second"})
	select {
	case <-subscription.overflow:
	default:
		t.Fatal("queue overflow silently discarded a message")
	}
	if streamCount(service) != 0 {
		t.Fatal("overflowed subscriber remained registered")
	}
	if first := <-subscription.messages; first.GetMessageId() != "first" {
		t.Fatalf("queue overwrote an earlier message: %v", first)
	}
}

func TestPromptIDsNeverBecomeReplyStorageIDs(t *testing.T) {
	for _, kind := range []string{"memory", "sqlite"} {
		t.Run(kind, func(t *testing.T) {
			var store chat.MessageStore = chat.NewMemoryMessageStore()
			if kind == "sqlite" {
				var err error
				store, err = chat.NewSQLiteMessageStore(filepath.Join(t.TempDir(), "chat.db"))
				if err != nil {
					t.Fatal(err)
				}
			}
			core, err := chat.NewChatServer(store)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				if err := core.Close(); err != nil {
					t.Error(err)
				}
			})
			prompt, err := core.AppendMessageWithID(chat.Message{ID: "prompt", Time: time.Now(), Nick: "reader", Text: "question", Color: 32})
			if err != nil {
				t.Fatal(err)
			}
			service := &gRPCServer{chatServer: core}
			replies := make(map[string]replyState)
			if err := service.handleResponse(&pb.AIResponse{MessageId: prompt.ID, Nick: "Assistant", Text: "first", Format: pb.ResponseFormat_DELTA}, replies); err != nil {
				t.Fatalf("reply must have a separate storage ID: %v", err)
			}
			if err := service.handleResponse(&pb.AIResponse{MessageId: prompt.ID, Text: " second", IsFinal: true, Format: pb.ResponseFormat_DELTA}, replies); err != nil {
				t.Fatal(err)
			}
			prompts, responses := 0, 0
			for _, message := range core.Messages() {
				if message.ID == prompt.ID {
					prompts++
					if message.Text != "question" || message.Nick != "reader" {
						t.Fatalf("prompt was overwritten: %+v", message)
					}
				}
				if message.Nick == "Assistant" {
					responses++
					if message.ID == prompt.ID || message.Text != "first second" {
						t.Fatalf("reply must have a separate storage ID and append chunks: %+v", message)
					}
				}
			}
			if prompts != 1 || responses != 1 || len(replies) != 0 {
				t.Fatalf("reply lifecycle lost prompt or duplicated response: %d %d %d", prompts, responses, len(replies))
			}
		})
	}
}

func TestReplyFormatsAndUnicodeSurviveStorage(t *testing.T) {
	for _, kind := range []string{"memory", "sqlite"} {
		t.Run(kind, func(t *testing.T) {
			var store chat.MessageStore = chat.NewMemoryMessageStore()
			if kind == "sqlite" {
				var err error
				store, err = chat.NewSQLiteMessageStore(filepath.Join(t.TempDir(), "chat.db"))
				if err != nil {
					t.Fatal(err)
				}
			}
			core, err := chat.NewChatServer(store)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				if err := core.Close(); err != nil {
					t.Error(err)
				}
			})
			service := &gRPCServer{chatServer: core}
			replies := make(map[string]replyState)
			cases := []struct {
				name   string
				first  string
				second string
				format pb.ResponseFormat
				want   string
			}{
				{name: "legacy", first: "par", second: "partial", want: "partial"},
				{name: "delta", first: "par", second: "tial", format: pb.ResponseFormat_DELTA, want: "partial"},
				{name: "unicode", first: "100\u00a0km \u0645\u06cc\u200c\u0631\u0648\u0645 ", second: "\U0001f469\u200d\U0001f52c\u2028safe\x1b\u009b\u202e", format: pb.ResponseFormat_DELTA, want: "100\u00a0km \u0645\u06cc\u200c\u0631\u0648\u0645 \U0001f469\u200d\U0001f52c safe"},
			}
			for _, example := range cases {
				if err := service.handleResponse(&pb.AIResponse{MessageId: example.name, Nick: example.name, Text: example.first, Format: example.format}, replies); err != nil {
					t.Fatal(err)
				}
				if err := service.handleResponse(&pb.AIResponse{MessageId: example.name, Text: example.second, Format: example.format, IsFinal: true}, replies); err != nil {
					t.Fatal(err)
				}
				found := false
				for _, message := range core.Messages() {
					if message.Nick == example.name {
						if found || message.Text != example.want {
							t.Fatalf("reply format or Unicode text was altered: case=%s got=%q want=%q", example.name, message.Text, example.want)
						}
						found = true
					}
				}
				if !found {
					t.Fatalf("reply %s was not stored", example.name)
				}
			}
			storedText := func(id string) string {
				t.Helper()
				for _, message := range core.Messages() {
					if message.ID == id {
						return message.Text
					}
				}
				t.Fatalf("reply %s is missing from storage", id)
				return ""
			}
			for _, example := range []struct {
				name       string
				format     pb.ResponseFormat
				final      bool
				afterEmpty string
				want       string
			}{
				{name: "empty-snapshot", afterEmpty: "", want: "Answer"},
				{name: "empty-delta", format: pb.ResponseFormat_DELTA, afterEmpty: "Draft", want: "DraftAnswer"},
				{name: "empty-final-marker", final: true, afterEmpty: "Draft", want: "Draft"},
			} {
				if err := service.handleResponse(&pb.AIResponse{MessageId: example.name, Text: "Draft"}, replies); err != nil {
					t.Fatal(err)
				}
				reply, exists := replies[example.name]
				if !exists {
					t.Fatal("draft reply has no pending state")
				}
				if err := service.handleResponse(&pb.AIResponse{MessageId: example.name, Format: example.format, IsFinal: example.final}, replies); err != nil {
					t.Fatal(err)
				}
				if got := storedText(reply.id); got != example.afterEmpty {
					t.Fatalf("empty response did not update stored reply: case=%s got=%q want=%q", example.name, got, example.afterEmpty)
				}
				if !example.final {
					if pending := replies[example.name]; pending.id != reply.id || pending.text != example.afterEmpty {
						t.Fatalf("empty response did not update pending reply: case=%s pending=%+v", example.name, pending)
					}
					if err := service.handleResponse(&pb.AIResponse{MessageId: example.name, Text: "Answer", Format: pb.ResponseFormat_DELTA, IsFinal: true}, replies); err != nil {
						t.Fatal(err)
					}
				}
				if _, exists := replies[example.name]; exists {
					t.Fatalf("final response retained pending state: %s", example.name)
				}
				if got := storedText(reply.id); got != example.want {
					t.Fatalf("empty response changed the final reply: case=%s got=%q want=%q", example.name, got, example.want)
				}
			}
			count, err := store.GetMessageCount()
			if err != nil {
				t.Fatal(err)
			}
			if err := service.handleResponse(&pb.AIResponse{MessageId: "invalid", Text: "ignored", Format: pb.ResponseFormat(99)}, replies); status.Code(err) != codes.InvalidArgument {
				t.Fatalf("unknown response format accepted: %v", err)
			}
			after, err := store.GetMessageCount()
			if err != nil || after != count {
				t.Fatalf("invalid format changed history: %d %d %v", count, after, err)
			}
		})
	}
}

func TestFinalReplyDoesNotConsumeAnUnfinishedSlot(t *testing.T) {
	core, err := chat.NewChatServer(chat.NewMemoryMessageStore())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := core.Close(); err != nil {
			t.Error(err)
		}
	})
	service := &gRPCServer{chatServer: core}
	replies := make(map[string]replyState)
	for index := 0; index < 100; index++ {
		if err := service.handleResponse(&pb.AIResponse{MessageId: fmt.Sprintf("pending-%d", index), Text: "pending"}, replies); err != nil {
			t.Fatal(err)
		}
	}
	before, err := core.Store.GetMessageCount()
	if err != nil {
		t.Fatal(err)
	}
	if err := service.handleResponse(&pb.AIResponse{MessageId: "finished", Text: "complete", IsFinal: true}, replies); err != nil {
		t.Fatalf("final reply consumed an unfinished slot: %v", err)
	}
	after, err := core.Store.GetMessageCount()
	if err != nil || after != before+1 {
		t.Fatalf("final reply was not stored: before=%d after=%d err=%v", before, after, err)
	}
	if len(replies) != 100 {
		t.Fatalf("pending count changed after final reply: %d", len(replies))
	}
	if err := service.handleResponse(&pb.AIResponse{MessageId: "overflow", Text: "pending"}, replies); status.Code(err) != codes.ResourceExhausted {
		t.Fatalf("unfinished reply bound was lost: %v", err)
	}
}

func TestUnknownReplyFormatDoesNotFinalizeAnOpenReply(t *testing.T) {
	core, err := chat.NewChatServer(chat.NewMemoryMessageStore())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := core.Close(); err != nil {
			t.Error(err)
		}
	})
	service := &gRPCServer{chatServer: core}
	replies := make(map[string]replyState)
	if err := service.handleResponse(&pb.AIResponse{MessageId: "open", Text: "pending"}, replies); err != nil {
		t.Fatal(err)
	}
	if err := service.handleResponse(&pb.AIResponse{MessageId: "open", IsFinal: true, Format: pb.ResponseFormat(99)}, replies); status.Code(err) != codes.InvalidArgument {
		t.Fatalf("invalid format finalized an open reply: %v", err)
	}
	if len(replies) != 1 {
		t.Fatal("invalid format discarded reply state")
	}
}
