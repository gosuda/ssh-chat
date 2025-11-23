package types

import (
	"sync"
	"time"
)

// BanManager keeps a set of banned IP addresses.
type BanManager struct {
	mu     sync.RWMutex
	banned map[string]*Ban
	store  Store
}

type Store interface {
	SaveBan(ban *Ban) error
	RemoveBan(ban *Ban) error
}

type Ban struct {
	IP       string
	BannedBy string // Who banned the IP
	Reason   string
	At       time.Time
}


