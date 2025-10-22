package types
import (
	"time"
	"sync"
	"github.com/gliderlabs/ssh"
)
type Message struct {
	Time     time.Time
	Nick     string
	Text     string
	Color    int
	IP       string
	Mentions []string // List of mentioned usernames
}

type ChatServer struct {
	mu       sync.RWMutex
	messages []Message
	clients  map[*Client]struct{}
}

