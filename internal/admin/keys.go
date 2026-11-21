package admin

import (
	"context"
	"encoding/json"
	"net/http"
	"regexp"
	"time"

	"github.com/raven-clown/transponder/internal/auth"
	"github.com/raven-clown/transponder/internal/store"
)

const kindKeys = "keys"

var namePattern = regexp.MustCompile(`^[A-Za-z0-9._-]{1,64}$`)

type keyJSON struct {
	Name          string   `json:"name"`
	Role          string   `json:"role"`
	Teams         []string `json:"teams"`
	Disabled      bool     `json:"disabled"`
	ExpiresAt     *string  `json:"expires_at"`
	Last4         string   `json:"last4"`
	Version       int64    `json:"version"`
	Source        string   `json:"source"`
	ManagedByFile bool     `json:"managed_by_file"`
	CreatedAt     string   `json:"created_at"`
	UpdatedAt     string   `json:"updated_at"`
}

type keyWithSecretJSON struct {
	keyJSON
	Secret string `json:"secret"`
}

func toKeyJSON(k store.APIKey) keyJSON {
	teams := k.Teams
	if teams == nil {
		teams = []string{}
	}
	role, err := auth.ParseRole(k.Role)
	if err != nil {
		role = auth.RoleMember
	}
	return keyJSON{
		Name:          k.Name,
		Role:          string(role),
		Teams:         teams,
		Disabled:      k.Disabled,
		ExpiresAt:     optTime(k.ExpiresAt),
		Last4:         k.Last4,
		Version:       k.Version,
		Source:        k.Source,
		ManagedByFile: k.Source == store.SourceFile,
		CreatedAt:     formatTime(k.CreatedAt),
		UpdatedAt:     formatTime(k.UpdatedAt),
	}
}

func (h *handler) listKeys(w http.ResponseWriter, r *http.Request) {
	p, err := readPage(r)
	if err != nil {
		h.fail(w, r, "keys", err)
		return
	}
	id, _ := auth.FromContext(r.Context())
	keys, err := h.d.Store.ListKeys(r.Context(), orgOf(id))
	if err != nil {
		h.fail(w, r, "keys", err)
		return
	}
	items := make([]keyJSON, 0, len(keys))
	for _, k := range keys {
		items = append(items, toKeyJSON(k))
	}
	writeJSON(w, http.StatusOK, keyPage(items, p, func(k keyJSON) string { return k.Name }))
}

func (h *handler) getKey(w http.ResponseWriter, r *http.Request) {
	id, _ := auth.FromContext(r.Context())
	k, err := h.d.Store.KeyByName(r.Context(), orgOf(id), r.PathValue("name"))
	if err != nil {
		h.fail(w, r, "key "+r.PathValue("name"), err)
		return
	}
	writeVersioned(w, http.StatusOK, k.Version, toKeyJSON(k))
}

type keyCreate struct {
	Name      string          `json:"name"`
	Role      string          `json:"role"`
	Teams     []string        `json:"teams"`
	Disabled  bool            `json:"disabled"`
	ExpiresAt json.RawMessage `json:"expires_at"`
}

func (h *handler) createKey(w http.ResponseWriter, r *http.Request) {
	var body keyCreate
	if err := decode(w, r, &body); err != nil {
		h.fail(w, r, "key", err)
		return
	}
	if !namePattern.MatchString(body.Name) {
		h.fail(w, r, "key", invalid("name", "name must be 1 to 64 letters, digits, dots, dashes or underscores"))
		return
	}
	role, err := auth.ParseRole(body.Role)
	if err != nil {
		h.fail(w, r, "key", invalid("role", "role must be admin, viewer or member"))
		return
	}
	expires, err := readExpiry(body.ExpiresAt)
	if err != nil {
		h.fail(w, r, "key", err)
		return
	}
	if now := h.d.Now(); !expires.IsZero() && !expires.After(now) {
		h.fail(w, r, "key", invalid("expires_at", "expires_at is in the past; a key that has expired cannot be used"))
		return
	}
	teams, err := readTeams(body.Teams)
	if err != nil {
		h.fail(w, r, "key", err)
		return
	}
	plain, hash, last4, err := auth.NewKey()
	if err != nil {
		h.fail(w, r, "key", err)
		return
	}
	id, _ := auth.FromContext(r.Context())
	now := h.d.Now()
	k := store.APIKey{
		OrgID:     orgOf(id),
		Name:      body.Name,
		Hash:      hash,
		Last4:     last4,
		Role:      string(role),
		Teams:     teams,
		Disabled:  body.Disabled,
		ExpiresAt: expires,
		CreatedAt: now,
		UpdatedAt: now,
		Source:    store.SourceAPI,
	}
	var created store.APIKey
	err = h.commit(r, change{kind: kindKeys, resource: k.Name, action: actionCreate,
		apply: func(ctx context.Context) (any, any, error) {
			if err := h.d.Store.CreateKey(ctx, k); err != nil {
				return nil, nil, err
			}
			var err error
			created, err = h.d.Store.KeyByName(ctx, k.OrgID, k.Name)
			return nil, toKeyJSON(created), err
		}})
	if err != nil {
		h.fail(w, r, "key "+k.Name, err)
		return
	}
	writeVersioned(w, http.StatusCreated, created.Version, keyWithSecretJSON{toKeyJSON(created), plain})
}

type keyPatch struct {
	Role      *string         `json:"role"`
	Teams     *[]string       `json:"teams"`
	Disabled  *bool           `json:"disabled"`
	ExpiresAt json.RawMessage `json:"expires_at"`
}

func (h *handler) updateKey(w http.ResponseWriter, r *http.Request) {
	version, err := ifMatch(r)
	if err != nil {
		h.fail(w, r, "key", err)
		return
	}
	var body keyPatch
	if err := decode(w, r, &body); err != nil {
		h.fail(w, r, "key", err)
		return
	}
	var role auth.Role
	if body.Role != nil {
		if role, err = auth.ParseRole(*body.Role); err != nil || *body.Role == "" {
			h.fail(w, r, "key", invalid("role", "role must be admin, viewer or member"))
			return
		}
	}
	var teams []string
	if body.Teams != nil {
		if teams, err = readTeams(*body.Teams); err != nil {
			h.fail(w, r, "key", err)
			return
		}
	}
	expires, err := readExpiry(body.ExpiresAt)
	if err != nil {
		h.fail(w, r, "key", err)
		return
	}
	name := r.PathValue("name")
	updated, err := h.changeKey(r, name, version, actionUpdate, func(k *store.APIKey) {
		if body.Role != nil {
			k.Role = string(role)
		}
		if body.Teams != nil {
			k.Teams = teams
		}
		if body.Disabled != nil {
			k.Disabled = *body.Disabled
		}
		if body.ExpiresAt != nil {
			k.ExpiresAt = expires
		}
	})
	if err != nil {
		h.fail(w, r, "key "+name, err)
		return
	}
	writeVersioned(w, http.StatusOK, updated.Version, toKeyJSON(updated))
}

func (h *handler) rotateKey(w http.ResponseWriter, r *http.Request) {
	version, err := ifMatch(r)
	if err != nil {
		h.fail(w, r, "key", err)
		return
	}
	plain, hash, last4, err := auth.NewKey()
	if err != nil {
		h.fail(w, r, "key", err)
		return
	}
	name := r.PathValue("name")
	updated, err := h.changeKey(r, name, version, actionRotate, func(k *store.APIKey) {
		k.Hash, k.Last4 = hash, last4
	})
	if err != nil {
		h.fail(w, r, "key "+name, err)
		return
	}
	writeVersioned(w, http.StatusOK, updated.Version, keyWithSecretJSON{toKeyJSON(updated), plain})
}

// changeKey reads the key, checks it may be changed, applies edit and stores it, all in one
// transaction with the audit record.
func (h *handler) changeKey(r *http.Request, name string, version int64, action string, edit func(*store.APIKey)) (store.APIKey, error) {
	id, _ := auth.FromContext(r.Context())
	var updated store.APIKey
	err := h.commit(r, change{kind: kindKeys, resource: name, action: action,
		apply: func(ctx context.Context) (any, any, error) {
			k, err := h.editable(ctx, orgOf(id), name, version)
			if err != nil {
				return nil, nil, err
			}
			before := toKeyJSON(k)
			edit(&k)
			k.UpdatedAt = h.d.Now()
			if updated, err = h.d.Store.UpdateKey(ctx, k, k.Version); err != nil {
				return nil, nil, err
			}
			return before, toKeyJSON(updated), nil
		}})
	return updated, err
}

func (h *handler) deleteKey(w http.ResponseWriter, r *http.Request) {
	version, err := ifMatch(r)
	if err != nil {
		h.fail(w, r, "key", err)
		return
	}
	id, _ := auth.FromContext(r.Context())
	name := r.PathValue("name")
	err = h.commit(r, change{kind: kindKeys, resource: name, action: actionDelete,
		apply: func(ctx context.Context) (any, any, error) {
			k, err := h.editable(ctx, orgOf(id), name, version)
			if err != nil {
				return nil, nil, err
			}
			return toKeyJSON(k), nil, h.d.Store.DeleteKey(ctx, k.OrgID, k.Name, k.Version)
		}})
	if err != nil {
		h.fail(w, r, "key "+name, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// editable reads a key inside the write's transaction and refuses one from the file or one
// whose version is not the one the caller read.
func (h *handler) editable(ctx context.Context, org, name string, version int64) (store.APIKey, error) {
	k, err := h.d.Store.KeyByName(ctx, org, name)
	if err != nil {
		return k, err
	}
	if k.Source == store.SourceFile {
		return k, managedByFile("key " + name)
	}
	if version > 0 && k.Version != version {
		return k, store.ErrVersionConflict
	}
	return k, nil
}

func managedByFile(what string) *apiError {
	return &apiError{status: http.StatusConflict, code: "managed_by_file",
		message: what + " is declared in the YAML file; change it there"}
}

// readExpiry reads an optional RFC 3339 time, where null or absent is no expiry.
// An explicit zero timestamp is refused so it cannot pass for "no expiry".
func readExpiry(raw json.RawMessage) (time.Time, error) {
	if len(raw) == 0 || string(raw) == "null" {
		return time.Time{}, nil
	}
	var s string
	if err := json.Unmarshal(raw, &s); err != nil {
		return time.Time{}, invalid("expires_at", "expires_at must be an RFC 3339 time or null")
	}
	t, err := time.Parse(time.RFC3339Nano, s)
	if err != nil {
		return time.Time{}, invalid("expires_at", "expires_at must be an RFC 3339 time or null")
	}
	if t.IsZero() {
		return time.Time{}, invalid("expires_at", "expires_at must not be the zero time; use null for no expiry")
	}
	return t, nil
}

func readTeams(teams []string) ([]string, error) {
	for _, t := range teams {
		if t == "" {
			return nil, invalid("teams", "teams must not hold an empty name")
		}
	}
	if len(teams) == 0 {
		return nil, nil
	}
	return teams, nil
}
