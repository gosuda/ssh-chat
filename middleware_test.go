package main

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/binary"
	"encoding/pem"
	"math/big"
	"net"
	"os"
	"path/filepath"
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
			if err := stream.Send(&pb.AIResponse{MessageId: "response", Nick: "Assistant", Text: "part"}); err != nil {
				t.Fatal(err)
			}
			await(t, func() bool {
				for _, m := range core.Messages() {
					if m.ID == "response" && m.Text == "part" {
						return true
					}
				}
				return false
			})
			if err := stream.Send(&pb.AIResponse{MessageId: "response", Text: " \ncomplete\nanswer"}); err != nil {
				t.Fatal(err)
			}
			await(t, func() bool {
				for _, m := range core.Messages() {
					if m.ID == "response" && m.Text == "complete answer" {
						return true
					}
				}
				return false
			})
			core.AppendSystemMessage("administration")
			core.AppendUserMessage(chat.Message{Time: time.Now(), Nick: "reader", Text: "next", Color: 32})
			next, err := stream.Recv()
			if err != nil {
				t.Fatal(err)
			}
			if next.Text != "next" || next.MessageId == first.MessageId {
				t.Fatalf("response loop or unstable ID: %v", next)
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
	service := &gRPCServer{chatServer: core, activeStreams: make(map[string]chan *pb.ChatMessage), clientPubKey: &key.PublicKey, authEnabled: true}
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
	if err := valid.Send(&pb.AIResponse{Ip: "127.0.0.1", AuthTimestamp: timestamp, AuthSignature: signAuthentication(t, key, "127.0.0.1", timestamp)}); err != nil {
		t.Fatal(err)
	}
	await(t, func() bool { return streamCount(service) == 1 })
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
	for _, flag := range []string{"addr", "host-key", "max-per-ip", "shutdown-countdown", "db-path", "sqlite", "ssh-port", "grpc-port", "grpc-security", "grpc-auth"} {
		if command.Flags().Lookup(flag) == nil {
			t.Fatalf("lost option --%s", flag)
		}
	}
	if grpcPort != "3333" || sshPort != "2222" || grpcSecurityMode != "none" || grpcAuthEnabled {
		t.Fatal("middleware defaults changed")
	}
	if got := sanitizeNickname("\x1breader\n"); got != "reader" {
		t.Fatalf("nickname sanitizer lost: %q", got)
	}
}
