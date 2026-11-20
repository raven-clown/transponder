package admin

import (
	"context"
	"crypto/rand"
	"encoding/base32"
	"fmt"
	"net/http"
	"strings"

	"github.com/raven-clown/transponder/internal/auth"
	"github.com/raven-clown/transponder/internal/store"
)

// itemSpec is one kind of runtime config item and how the API reads and writes it.
type itemSpec struct {
	kind  string
	what  string // the name of one item in messages, such as provider
	param string // the path parameter: name, or id when ids are generated
	// prefix starts a generated id. Empty when the body names the item.
	prefix string
	view   func(it store.ConfigItem) (any, error)
	// prepare reads a create body into a new item: its id when the body names it, body,
	// secret and position.
	prepare func(raw []byte) (store.ConfigItem, error)
	// finish checks a new or changed item against the others, inside the write.
	finish func(ctx context.Context, it *store.ConfigItem) error
	// patch applies a PATCH body to the stored item.
	patch func(it *store.ConfigItem, raw []byte) error
	// deletable checks nothing still needs the item.
	deletable func(ctx context.Context, it store.ConfigItem) error
}

type metaJSON struct {
	Version       int64  `json:"version"`
	Source        string `json:"source"`
	ManagedByFile bool   `json:"managed_by_file"`
	CreatedAt     string `json:"created_at"`
	UpdatedAt     string `json:"updated_at"`
}

func metaOf(it store.ConfigItem) metaJSON {
	return metaJSON{
		Version:       it.Version,
		Source:        it.Source,
		ManagedByFile: it.Source == store.SourceFile,
		CreatedAt:     formatTime(it.CreatedAt),
		UpdatedAt:     formatTime(it.UpdatedAt),
	}
}

// itemKey sorts items as the store does, by position and then id, as one string for cursors.
func itemKey(it store.ConfigItem) string { return fmt.Sprintf("%019d/%s", it.Position, it.ID) }

func (h *handler) itemRoutes(s *itemSpec) []route {
	base := "/" + s.kind
	one := base + "/{" + s.param + "}"
	return []route{
		{http.MethodGet, base, auth.RoleViewer, h.listItems(s)},
		{http.MethodPost, base, auth.RoleAdmin, h.createItem(s)},
		{http.MethodGet, one, auth.RoleViewer, h.getItem(s)},
		{http.MethodPatch, one, auth.RoleAdmin, h.updateItem(s)},
		{http.MethodDelete, one, auth.RoleAdmin, h.deleteItem(s)},
	}
}

func (h *handler) listItems(s *itemSpec) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		p, err := readPage(r)
		if err != nil {
			h.fail(w, r, s.kind, err)
			return
		}
		id, _ := auth.FromContext(r.Context())
		items, err := h.d.Store.ListConfigItems(r.Context(), orgOf(id), s.kind)
		if err != nil {
			h.fail(w, r, s.kind, err)
			return
		}
		pg := keyPage(items, p, itemKey)
		out := listJSON[any]{Items: make([]any, 0, len(pg.Items)), NextCursor: pg.NextCursor}
		for _, it := range pg.Items {
			v, err := s.view(it)
			if err != nil {
				h.fail(w, r, s.kind, err)
				return
			}
			out.Items = append(out.Items, v)
		}
		writeJSON(w, http.StatusOK, out)
	}
}

func (h *handler) getItem(s *itemSpec) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id, _ := auth.FromContext(r.Context())
		name := r.PathValue(s.param)
		it, err := h.d.Store.ConfigItem(r.Context(), orgOf(id), s.kind, name)
		if err != nil {
			h.fail(w, r, s.what+" "+name, err)
			return
		}
		v, err := s.view(it)
		if err != nil {
			h.fail(w, r, s.what+" "+name, err)
			return
		}
		writeVersioned(w, http.StatusOK, it.Version, v)
	}
}

func (h *handler) createItem(s *itemSpec) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		raw, err := readBody(w, r)
		if err != nil {
			h.fail(w, r, s.what, err)
			return
		}
		it, err := s.prepare(raw)
		if err != nil {
			h.fail(w, r, s.what, err)
			return
		}
		if s.prefix != "" {
			if it.ID, err = newID(s.prefix); err != nil {
				h.fail(w, r, s.what, err)
				return
			}
		}
		id, _ := auth.FromContext(r.Context())
		now := h.d.Now()
		it.OrgID, it.Kind, it.Source, it.CreatedAt, it.UpdatedAt = orgOf(id), s.kind, store.SourceAPI, now, now
		var view any
		var stored store.ConfigItem
		err = h.commit(r, change{kind: s.kind, resource: it.ID, action: actionCreate,
			apply: func(ctx context.Context) (any, any, error) {
				if s.finish != nil {
					if err := s.finish(ctx, &it); err != nil {
						return nil, nil, err
					}
				}
				if err := h.d.Store.CreateConfigItem(ctx, it); err != nil {
					return nil, nil, err
				}
				if err := h.validate(ctx); err != nil {
					return nil, nil, err
				}
				var err error
				if stored, err = h.d.Store.ConfigItem(ctx, it.OrgID, it.Kind, it.ID); err != nil {
					return nil, nil, err
				}
				view, err = s.view(stored)
				return nil, view, err
			}})
		if err != nil {
			h.fail(w, r, s.what+" "+it.ID, err)
			return
		}
		writeVersioned(w, http.StatusCreated, stored.Version, view)
	}
}

func (h *handler) updateItem(s *itemSpec) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		version, err := ifMatch(r)
		if err != nil {
			h.fail(w, r, s.what, err)
			return
		}
		raw, err := readBody(w, r)
		if err != nil {
			h.fail(w, r, s.what, err)
			return
		}
		id, _ := auth.FromContext(r.Context())
		name := r.PathValue(s.param)
		var view any
		var stored store.ConfigItem
		err = h.commit(r, change{kind: s.kind, resource: name, action: actionUpdate,
			apply: func(ctx context.Context) (any, any, error) {
				it, err := h.editableItem(ctx, s, orgOf(id), name, version)
				if err != nil {
					return nil, nil, err
				}
				before, err := s.view(it)
				if err != nil {
					return nil, nil, err
				}
				if err := s.patch(&it, raw); err != nil {
					return nil, nil, err
				}
				if s.finish != nil {
					if err := s.finish(ctx, &it); err != nil {
						return nil, nil, err
					}
				}
				it.UpdatedAt = h.d.Now()
				if stored, err = h.d.Store.UpdateConfigItem(ctx, it, it.Version); err != nil {
					return nil, nil, err
				}
				if err := h.validate(ctx); err != nil {
					return nil, nil, err
				}
				view, err = s.view(stored)
				return before, view, err
			}})
		if err != nil {
			h.fail(w, r, s.what+" "+name, err)
			return
		}
		writeVersioned(w, http.StatusOK, stored.Version, view)
	}
}

func (h *handler) deleteItem(s *itemSpec) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		version, err := ifMatch(r)
		if err != nil {
			h.fail(w, r, s.what, err)
			return
		}
		id, _ := auth.FromContext(r.Context())
		name := r.PathValue(s.param)
		err = h.commit(r, change{kind: s.kind, resource: name, action: actionDelete,
			apply: func(ctx context.Context) (any, any, error) {
				it, err := h.editableItem(ctx, s, orgOf(id), name, version)
				if err != nil {
					return nil, nil, err
				}
				before, err := s.view(it)
				if err != nil {
					return nil, nil, err
				}
				if s.deletable != nil {
					if err := s.deletable(ctx, it); err != nil {
						return nil, nil, err
					}
				}
				if err := h.d.Store.DeleteConfigItem(ctx, it.OrgID, it.Kind, it.ID, it.Version); err != nil {
					return nil, nil, err
				}
				return before, nil, h.validate(ctx)
			}})
		if err != nil {
			h.fail(w, r, s.what+" "+name, err)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}
}

// editableItem reads an item inside the write's transaction and refuses one from the file
// or one whose version is not the one the caller read.
func (h *handler) editableItem(ctx context.Context, s *itemSpec, org, name string, version int64) (store.ConfigItem, error) {
	it, err := h.d.Store.ConfigItem(ctx, org, s.kind, name)
	if err != nil {
		return it, err
	}
	if it.Source == store.SourceFile {
		return it, managedByFile(s.what + " " + name)
	}
	if version > 0 && it.Version != version {
		return it, store.ErrVersionConflict
	}
	return it, nil
}

// validate loads the whole runtime config as the write leaves it, inside the write's
// transaction, so a write that would leave a config that does not load is rolled back and
// the old one keeps serving.
func (h *handler) validate(ctx context.Context) error {
	if _, err := Runtime(ctx, h.d.Store, h.d.Secrets, h.d.File); err != nil {
		return &apiError{status: http.StatusBadRequest, code: "invalid_config",
			message: "the change would leave a config that does not load: " + strings.TrimPrefix(err.Error(), "config: ")}
	}
	return nil
}

var idEncoding = base32.NewEncoding("abcdefghijklmnopqrstuvwxyz234567").WithPadding(base32.NoPadding)

func newID(prefix string) (string, error) {
	b := make([]byte, 8)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return prefix + idEncoding.EncodeToString(b), nil
}
