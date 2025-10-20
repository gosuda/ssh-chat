package main
import "./types"

func NewConnectionRateLimiter() *ConnectionRateLimiter {
	return &ConnectionRateLimiter{
		entries: make(map[string][]time.Time),
	}
}

// CheckAndRecord returns true if the connection should be allowed, false otherwise.
func (rl *ConnectionRateLimiter) CheckAndRecord(ip string) bool {
	rl.mu.Lock()
	defer rl.mu.Unlock()

	now := time.Now()
	oneMinuteAgo := now.Add(-1 * time.Minute)

	timestamps := rl.entries[ip]

	newTimestamps := make([]time.Time, 0, len(timestamps))
	for _, ts := range timestamps {
		if ts.After(oneMinuteAgo) {
			newTimestamps = append(newTimestamps, ts)
		}
	}

	if len(newTimestamps) >= 5 {
		return false
	}

	newTimestamps = append(newTimestamps, now)
	rl.entries[ip] = newTimestamps
	return true
}
