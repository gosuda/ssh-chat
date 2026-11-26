package types

import (
	"sync"
	"time"
)

// ConnectionRateLimiter tracks connection attempts per IP.
type ConnectionRateLimiter struct {
	mu      sync.Mutex
	entries map[string][]time.Time
}
