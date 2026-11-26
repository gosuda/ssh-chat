package chat

import (
	"path/filepath"
	"sync"
	"testing"
	"time"
)

func findMessage(t *testing.T, server *ChatServer, id string) Message {
	t.Helper()
	for _, message := range server.Messages() {
		if message.ID == id {
			return message
		}
	}
	t.Fatalf("message %q was not stored", id)
	return Message{}
}

func TestStreamedMessagesUseTheSelectedStore(t *testing.T) {
	for _, kind := range []string{"memory", "sqlite"} {
		t.Run(kind, func(t *testing.T) {
			var store MessageStore = NewMemoryMessageStore()
			if kind == "sqlite" {
				var err error
				store, err = NewSQLiteMessageStore(filepath.Join(t.TempDir(), "chat.db"))
				if err != nil {
					t.Fatal(err)
				}
			}
			server, err := NewChatServer(store)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				if err := server.Close(); err != nil {
					t.Error(err)
				}
			})
			var delivered []Message
			server.SetUserMessageHandler(func(message Message) { delivered = append(delivered, message) })
			client := NewClient(server, server.Bans, store, nil, "reader", 80, 24, 32, "192.0.2.1")
			defer client.Close()
			client.inputBuffer = []rune("question")
			client.handleEnter()
			if len(delivered) != 1 || delivered[0].ID == "" {
				t.Fatalf("user message lacks a stable broadcast ID: %+v", delivered)
			}
			stored := findMessage(t, server, delivered[0].ID)
			if stored.Text != delivered[0].Text || stored.IP != delivered[0].IP {
				t.Fatalf("broadcast and stored message differ: %+v %+v", stored, delivered[0])
			}
			if _, err := server.AppendMessageWithID(Message{ID: "response", Time: time.Now(), Nick: "Assistant", Text: "part", Color: 33}); err != nil {
				t.Fatal(err)
			}
			updated, err := server.UpdateMessage("response", " \ncomplete\nanswer @reader\r")
			if err != nil || !updated {
				t.Fatalf("update failed: updated=%v err=%v", updated, err)
			}
			stored = findMessage(t, server, "response")
			if stored.Text != "complete answer @reader" || len(stored.Mentions) != 1 || stored.Mentions[0] != "reader" {
				t.Fatalf("streamed update was not normalized or persisted: %+v", stored)
			}
			server.AppendSystemMessage("administration")
			if len(delivered) != 1 {
				t.Fatalf("middleware or system messages were broadcast back to the middleware: %+v", delivered)
			}
			if updated, err := server.UpdateMessage("missing", "ignored"); err != nil || updated {
				t.Fatalf("missing ID: updated=%v err=%v", updated, err)
			}
		})
	}
}

func TestSQLiteMessageIDsMigrateAndPersist(t *testing.T) {
	path := filepath.Join(t.TempDir(), "legacy.db")
	store, err := NewSQLiteMessageStore(path)
	if err != nil {
		t.Fatal(err)
	}
	_, err = store.db.Exec("CREATE TABLE messages (id INTEGER PRIMARY KEY AUTOINCREMENT, timestamp DATETIME NOT NULL, nick TEXT NOT NULL, text TEXT NOT NULL, color INTEGER NOT NULL, ip TEXT, mentions TEXT)")
	if err != nil {
		t.Fatal(err)
	}
	_, err = store.db.Exec("INSERT INTO messages(timestamp,nick,text,color) VALUES(?,?,?,?)", time.Now(), "legacy", "history", 31)
	if err != nil {
		t.Fatal(err)
	}
	server, err := NewChatServer(store)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.CreateUser("reader", 31); err != nil {
		t.Fatal(err)
	}
	if err := store.SetUserColor("reader", 34); err != nil {
		t.Fatal(err)
	}
	message, err := server.AppendMessageWithID(Message{ID: "durable-response", Time: time.Now(), Nick: "Assistant", Text: "part", Color: 33})
	if err != nil {
		t.Fatal(err)
	}
	if updated, err := server.UpdateMessage(message.ID, "complete"); err != nil || !updated {
		t.Fatalf("persist update: %v %v", updated, err)
	}
	if err := server.Close(); err != nil {
		t.Fatal(err)
	}
	store, err = NewSQLiteMessageStore(path)
	if err != nil {
		t.Fatal(err)
	}
	server, err = NewChatServer(store)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := server.Close(); err != nil {
			t.Error(err)
		}
	})
	if got := findMessage(t, server, "durable-response").Text; got != "complete" {
		t.Fatalf("update lost after reopen: %q", got)
	}
	legacy := false
	for _, message := range server.Messages() {
		legacy = legacy || message.Nick == "legacy" && message.Text == "history"
	}
	if !legacy {
		t.Fatal("schema migration lost legacy history")
	}
	color, err := store.GetUserColor("reader")
	if err != nil || color != 34 {
		t.Fatalf("user storage lost: color=%d err=%v", color, err)
	}
}

func TestMemorySnapshotsRemainStableDuringUpdates(t *testing.T) {
	store := NewMemoryMessageStore()
	if err := store.AppendMessage(Message{ID: "response", Text: "before"}); err != nil {
		t.Fatal(err)
	}
	snapshot, err := store.GetMessages(0, 100)
	if err != nil {
		t.Fatal(err)
	}
	var group sync.WaitGroup
	group.Add(2)
	go func() {
		defer group.Done()
		for index := 0; index < 1000; index++ {
			if _, err := store.UpdateMessage("response", "after", []string{"reader"}); err != nil {
				t.Error(err)
				return
			}
		}
	}()
	go func() {
		defer group.Done()
		for index := 0; index < 1000; index++ {
			messages, err := store.GetMessages(0, 100)
			if err != nil {
				t.Error(err)
				return
			}
			for _, message := range messages {
				if message.ID != "response" {
					t.Error("unstable message ID")
					return
				}
			}
		}
	}()
	group.Wait()
	if snapshot[0].Text != "before" {
		t.Fatalf("snapshot changed after an update: %+v", snapshot)
	}
}
