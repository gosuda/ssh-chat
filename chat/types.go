package chat

import (
	"time"
)

type Message struct {
	Time     time.Time
	Nick     string
	Text     string
	Color    int
	IP       string
	Mentions []string // List of mentioned usernames
}
