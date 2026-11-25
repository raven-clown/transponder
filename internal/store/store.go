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
	// ErrVersionConflict means the record changed since the version the caller read.
	ErrVersionConflict = errors.New("store: version conflict")
)

// Where a config item came from: the admin API, or the YAML file, which the API may not change.
const (
	SourceAPI  = "api"
	SourceFile = "file"
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
	// Version starts at 1 and every update raises it. CreateKey ignores it.
	Version   int64
	Source    string
	UpdatedAt time.Time
}

// KeyStore stores API keys.
type KeyStore interface {
	CreateKey(ctx context.Context, k APIKey) error
	KeyByHash(ctx context.Context, hash [32]byte) (APIKey, error)
	HasAdminKey(ctx context.Context, orgID string, now time.Time) (bool, error)
}

// KeyAdminStore is what the admin API needs to manage keys.
type KeyAdminStore interface {
	KeyStore
	KeyByName(ctx context.Context, orgID, name string) (APIKey, error)
	// ListKeys returns orgID's keys sorted by name.
	ListKeys(ctx context.Context, orgID string) ([]APIKey, error)
	// UpdateKey stores k's hash, last4, role, teams, disabled flag and expiry, and raises its
	// version. With version above zero it returns ErrVersionConflict unless the stored version
	// is that one. It returns the key as stored.
	UpdateKey(ctx context.Context, k APIKey, version int64) (APIKey, error)
	// DeleteKey removes a key, checking version as UpdateKey does.
	DeleteKey(ctx context.Context, orgID, name string, version int64) error
}

// ConfigItem is one item of the runtime config: a provider, route, budget or profile. Body is
// the item as JSON, which the admin API reads and validates; the store keeps it as it is.
type ConfigItem struct {
	OrgID string
	Kind  string // providers, routes, budgets or profiles
	ID    string // the name, or a generated id
	// Position orders the items of a kind, then ID. Routes are tried in this order.
	Position int64
	Body     []byte
	// Secret is a sealed secret, such as a provider's API key. Empty when there is none.
	Secret    string
	Version   int64 // starts at 1; CreateConfigItem ignores it
	Source    string
	CreatedAt time.Time
	UpdatedAt time.Time
}

// ConfigItemStore keeps the runtime config items.
type ConfigItemStore interface {
	// ListConfigItems returns orgID's items of kind, by position and then id.
	ListConfigItems(ctx context.Context, orgID, kind string) ([]ConfigItem, error)
	ConfigItem(ctx context.Context, orgID, kind, id string) (ConfigItem, error)
	CreateConfigItem(ctx context.Context, it ConfigItem) error
	// UpdateConfigItem stores it's position, body, secret, source and updated time and raises
	// its version. With version above zero it returns ErrVersionConflict unless the stored
	// version is that one. It returns the item as stored.
	UpdateConfigItem(ctx context.Context, it ConfigItem, version int64) (ConfigItem, error)
	// DeleteConfigItem removes an item, checking version as UpdateConfigItem does.
	DeleteConfigItem(ctx context.Context, orgID, kind, id string, version int64) error
}

// AuditRecord is one write through the admin API.
type AuditRecord struct {
	ID        int64
	OrgID     string
	Time      time.Time
	Actor     string // key:<name> or user:<id>
	Action    string // create, update, delete, rotate
	Kind      string // the resource kind, such as keys or providers
	Resource  string // the resource name or id
	Diff      []byte // JSON, with secrets replaced
	RequestID string
}

// AuditQuery selects audit records, newest first. Empty fields match everything.
type AuditQuery struct {
	OrgID    string
	Actor    string
	Action   string // create, update, delete or rotate
	Kind     string
	Resource string
	From     time.Time // inclusive
	To       time.Time // exclusive
	BeforeID int64     // only records with a smaller id, for the next page
	Limit    int
}

// AuditStore keeps the audit log. Records are never changed or deleted.
type AuditStore interface {
	AddAudit(ctx context.Context, r AuditRecord) error
	ListAudit(ctx context.Context, q AuditQuery) ([]AuditRecord, error)
}

// UsageRecord is one call through the gateway.
type UsageRecord struct {
	// ID is set by ListUsage only.
	ID               int64
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
	// UsageSince calls fn for each of orgID's records from since on, oldest first.
	UsageSince(ctx context.Context, orgID string, since time.Time, fn func(UsageRecord) error) error
}

// UsageQuery selects usage records, newest first. Empty fields match everything.
type UsageQuery struct {
	OrgID    string
	From     time.Time // inclusive
	To       time.Time // exclusive
	User     string
	Key      string
	Project  string
	BeforeID int64 // only records with a smaller id, for the next page
	Limit    int
}

// UsageAdminStore is what the admin API reads usage with.
type UsageAdminStore interface {
	UsageStore
	// ListUsage returns the records q selects, with their ids, newest first.
	ListUsage(ctx context.Context, q UsageQuery) ([]UsageRecord, error)
}

// ConfigStore versions the runtime config, so every replica can tell when it changed.
type ConfigStore interface {
	// ConfigVersion returns the version of the runtime config. It starts at 1 and every
	// UpdateConfig raises it.
	ConfigVersion(ctx context.Context) (int64, error)
	// UpdateConfig runs fn in one transaction and raises the config version in it, so a replica
	// reloads once the change commits and never sees half of it. Store calls fn makes with the
	// ctx it is given join the transaction; an error from fn rolls everything back. It returns
	// the new version.
	UpdateConfig(ctx context.Context, fn func(ctx context.Context) error) (int64, error)
}
