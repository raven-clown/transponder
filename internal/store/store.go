// Package store defines the persistence interfaces transponder uses.
package store

import (
	"context"
	"errors"
	"time"
)

// DefaultOrg is the organisation every record belongs to until organisations ship.
const DefaultOrg = "default"

var (
	// ErrNotFound means the requested record does not exist.
	ErrNotFound = errors.New("store: not found")
	// ErrConflict means a record with the same identity already exists.
	ErrConflict = errors.New("store: conflict")
)

// APIKey is a stored API key. Only the hash of the secret is kept.
type APIKey struct {
	OrgID     string
	Name      string
	Hash      [32]byte
	Last4     string
	Role      string
	Teams     []string
	Disabled  bool
	ExpiresAt time.Time
	CreatedAt time.Time
}

// KeyStore stores API keys.
type KeyStore interface {
	CreateKey(ctx context.Context, k APIKey) error
	KeyByHash(ctx context.Context, hash [32]byte) (APIKey, error)
	HasAdminKey(ctx context.Context, orgID string, now time.Time) (bool, error)
}

// UsageRecord is one call through the gateway.
type UsageRecord struct {
	OrgID            string
	Time             time.Time
	RequestID        string
	User             string
	Teams            []string
	Key              string
	Project          string
	Route            string
	Provider         string
	Model            string
	Endpoint         string
	InputTokens      int64
	OutputTokens     int64
	CacheReadTokens  int64
	CacheWriteTokens int64
	ReasoningTokens  int64
	CostNUSD         int64
	Estimated        bool
	Status           int
	Latency          time.Duration
	Fallbacks        int
}

// UsageStore stores usage records.
type UsageStore interface {
	RecordUsage(ctx context.Context, r UsageRecord) error
	RecentUsage(ctx context.Context, orgID string, limit int) ([]UsageRecord, error)
}
