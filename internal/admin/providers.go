package admin

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"strings"

	"github.com/raven-clown/transponder/internal/provider/registry"
	"github.com/raven-clown/transponder/internal/secrets"
	"github.com/raven-clown/transponder/internal/store"
)

type secretJSON struct {
	Set   bool   `json:"set"`
	Last4 string `json:"last4,omitempty"`
}

type providerJSON struct {
	Name    string     `json:"name"`
	Type    string     `json:"type"`
	BaseURL string     `json:"base_url"`
	APIKey  secretJSON `json:"api_key"`
	metaJSON
}

// providerItem is what the store keeps for a provider: the key set flag is kept apart from
// last4, since a short key has no last four characters to show.
type providerItem struct {
	providerBody
	KeySet bool `json:"api_key_set,omitempty"`
}

func viewProvider(it store.ConfigItem) (any, error) {
	var b providerItem
	if err := json.Unmarshal(it.Body, &b); err != nil {
		return nil, err
	}
	return providerJSON{
		Name:     it.ID,
		Type:     b.Type,
		BaseURL:  b.BaseURL,
		APIKey:   secretJSON{Set: b.KeySet || b.KeyLast4 != "", Last4: b.KeyLast4},
		metaJSON: metaOf(it),
	}, nil
}

type providerCreate struct {
	Name    string `json:"name"`
	Type    string `json:"type"`
	BaseURL string `json:"base_url"`
	APIKey  string `json:"api_key"`
}

type providerPatch struct {
	Type    *string         `json:"type"`
	BaseURL *string         `json:"base_url"`
	APIKey  json.RawMessage `json:"api_key"`
}

func (h *handler) providerSpec() *itemSpec {
	return &itemSpec{
		kind: kindProviders, what: "provider", param: "name",
		view: viewProvider,
		prepare: func(raw []byte) (store.ConfigItem, error) {
			var body providerCreate
			if err := decodeBytes(raw, &body); err != nil {
				return store.ConfigItem{}, err
			}
			if !namePattern.MatchString(body.Name) {
				return store.ConfigItem{}, invalid("name", "name must be 1 to 64 letters, digits, dots, dashes or underscores")
			}
			it := store.ConfigItem{ID: body.Name}
			b := providerItem{providerBody: providerBody{Type: body.Type, BaseURL: body.BaseURL}}
			if err := h.setProviderKey(&it, &b, body.APIKey); err != nil {
				return it, err
			}
			return it, putProvider(&it, b)
		},
		patch: func(it *store.ConfigItem, raw []byte) error {
			var body providerPatch
			if err := decodeBytes(raw, &body); err != nil {
				return err
			}
			var b providerItem
			if err := json.Unmarshal(it.Body, &b); err != nil {
				return err
			}
			if body.Type != nil {
				b.Type = *body.Type
			}
			if body.BaseURL != nil {
				b.BaseURL = *body.BaseURL
			}
			if body.APIKey != nil {
				var key *string
				if err := json.Unmarshal(body.APIKey, &key); err != nil {
					return invalid("api_key", "api_key must be a string, or null to remove it")
				}
				if err := h.setProviderKey(it, &b, deref(key)); err != nil {
					return err
				}
			}
			return putProvider(it, b)
		},
		deletable: func(ctx context.Context, it store.ConfigItem) error {
			routes, err := h.d.Store.ListConfigItems(ctx, it.OrgID, kindRoutes)
			if err != nil {
				return err
			}
			if ids := usedBy(routes, it.ID); len(ids) > 0 {
				return &apiError{status: http.StatusConflict, code: "in_use",
					message: "provider " + it.ID + " is used by routes " + strings.Join(ids, ", ") + "; change or delete them first"}
			}
			return nil
		},
	}
}

// setProviderKey seals key into it, or removes the key when key is empty.
func (h *handler) setProviderKey(it *store.ConfigItem, b *providerItem, key string) error {
	if key == "" {
		it.Secret, b.KeyLast4, b.KeySet = "", "", false
		return nil
	}
	sealed, err := h.d.Secrets.Seal(key, secretContext(it.ID))
	if errors.Is(err, secrets.ErrNoKey) {
		return &apiError{status: http.StatusBadRequest, code: "secrets_key_missing", field: "api_key",
			message: "provider keys are stored encrypted, and no secrets_key is set; set secrets_key or TRANSPONDER_SECRETS_KEY to the same value on every replica"}
	}
	if err != nil {
		return err
	}
	it.Secret, b.KeyLast4, b.KeySet = sealed, last4(key), true
	return nil
}

// putProvider checks b and stores it as the item's body.
func putProvider(it *store.ConfigItem, b providerItem) error {
	spec, ok := registry.Lookup(b.Type)
	if !ok {
		return invalid("type", "type %q is not known; known types are %s", b.Type, strings.Join(registry.Types(), ", "))
	}
	if spec.NeedsBaseURL && b.BaseURL == "" {
		return invalid("base_url", "a provider of type %s needs base_url", b.Type)
	}
	if b.BaseURL != "" {
		if u, err := url.Parse(b.BaseURL); err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
			return invalid("base_url", "base_url must be an http or https URL")
		}
	}
	raw, err := json.Marshal(b)
	it.Body = raw
	return err
}

func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}
