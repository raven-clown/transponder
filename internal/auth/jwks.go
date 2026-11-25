package auth

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/go-jose/go-jose/v4"
)

const (
	// jwksRefreshInterval is the shortest time between two fetches of the key set, so tokens
	// naming keys that do not exist cannot make transponder flood the identity provider.
	jwksRefreshInterval = 10 * time.Second
	// jwksMaxAge is how long a fetched key set is used before it is read again, so a key the
	// provider removed stops being trusted.
	jwksMaxAge = time.Hour
	// jwksFetchTimeout bounds one fetch, discovery included.
	jwksFetchTimeout = 10 * time.Second
	// jwksMaxBytes bounds the discovery document and the key set.
	jwksMaxBytes = 1 << 20
)

// keySet is the issuer's signing keys, fetched when first needed and cached.
type keySet struct {
	issuer  string
	jwksURL string // empty until discovery has run, unless it was configured
	client  *http.Client
	now     func() time.Time

	mu        sync.Mutex
	keys      []jose.JSONWebKey
	fetched   time.Time     // the last fetch that worked
	attempted time.Time     // the last fetch, whether or not it worked
	inflight  chan struct{} // closed when the running fetch ends
	lastErr   error         // the error of the last fetch, nil after one that worked
	lastErrAt time.Time     // when lastErr happened
}

// KeyState is the key set as this replica last read it. LastError is the error of the
// latest fetch and is empty once a fetch has worked. It holds no token.
type KeyState struct {
	Issuer      string
	Keys        int
	FetchedAt   time.Time // the last fetch that worked; zero before the first one
	LastError   string
	LastErrorAt time.Time
}

// state returns a snapshot of the key set.
func (s *keySet) state() KeyState {
	s.mu.Lock()
	defer s.mu.Unlock()
	st := KeyState{Issuer: s.issuer, Keys: len(s.keys), FetchedAt: s.fetched}
	if s.lastErr != nil {
		st.LastError, st.LastErrorAt = s.lastErr.Error(), s.lastErrAt
	}
	return st
}

// lookup returns the keys a token naming kid may be signed with: the key with that id, or
// every key when the token names none. An empty result with no error means the issuer has
// no such key.
//
// The key set is fetched when there is none yet, when kid is not in it or when it is older
// than jwksMaxAge, but never more often than jwksRefreshInterval. A caller waits for the
// fetch only when it has no key to go on: an old key set stays in use while it is read again
// and when the provider cannot be reached.
//
// With reread set, the cached keys are taken as not matching, so the set is read again as
// for an unknown kid: for a token without kid that no cached key verifies.
func (s *keySet) lookup(ctx context.Context, kid string, reread bool) ([]jose.JSONWebKey, error) {
	s.mu.Lock()
	for waited := false; ; waited = true {
		keys := s.match(kid)
		if reread && !waited {
			keys = nil
		}
		now := s.now()
		tooSoon := !s.attempted.IsZero() && now.Sub(s.attempted) < jwksRefreshInterval
		canFetch := s.inflight == nil && !tooSoon && !waited
		if len(keys) > 0 {
			if canFetch && now.Sub(s.fetched) >= jwksMaxAge {
				s.startRefresh(now)
			}
			s.mu.Unlock()
			return keys, nil
		}
		if s.inflight == nil && !canFetch {
			defer s.mu.Unlock()
			if s.fetched.IsZero() {
				return nil, fmt.Errorf("%w: %w", ErrUnavailable, s.lastErr)
			}
			return nil, nil
		}
		if s.inflight == nil {
			s.startRefresh(now)
		}
		done := s.inflight
		s.mu.Unlock()
		select {
		case <-done:
		case <-ctx.Done():
			return nil, fmt.Errorf("%w: waiting for the issuer's keys: %w", ErrUnavailable, ctx.Err())
		}
		s.mu.Lock()
	}
}

// startRefresh begins a fetch in the background. The caller holds mu.
func (s *keySet) startRefresh(now time.Time) {
	s.inflight = make(chan struct{})
	s.attempted = now
	go s.refresh()
}

// match returns the cached keys for kid. The caller holds mu.
func (s *keySet) match(kid string) []jose.JSONWebKey {
	if kid == "" {
		return s.keys
	}
	var out []jose.JSONWebKey
	for _, k := range s.keys {
		if k.KeyID == kid {
			out = append(out, k)
		}
	}
	return out
}

// refresh fetches the key set once and wakes the callers waiting for it. It runs on its own
// context, so a caller that gives up does not fail the fetch for the others.
func (s *keySet) refresh() {
	ctx, cancel := context.WithTimeout(context.Background(), jwksFetchTimeout)
	defer cancel()
	s.mu.Lock()
	jwksURL := s.jwksURL
	s.mu.Unlock()

	var keys []jose.JSONWebKey
	var err error
	if jwksURL == "" {
		jwksURL, err = s.discover(ctx)
	}
	if err == nil {
		keys, err = s.fetchKeys(ctx, jwksURL)
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	s.lastErr = err
	// The interval counts from the end of a fetch, so a provider that hangs is not asked
	// again the moment the fetch gives up.
	s.attempted = s.now()
	s.lastErrAt = s.attempted
	if err == nil {
		s.jwksURL, s.keys, s.fetched = jwksURL, keys, s.attempted
	}
	close(s.inflight)
	s.inflight = nil
}

// discover reads the address of the key set from the issuer's discovery document.
func (s *keySet) discover(ctx context.Context) (string, error) {
	var doc struct {
		Issuer  string `json:"issuer"`
		JWKSURI string `json:"jwks_uri"`
	}
	well := strings.TrimSuffix(s.issuer, "/") + "/.well-known/openid-configuration"
	if err := s.getJSON(ctx, well, &doc); err != nil {
		return "", err
	}
	if doc.Issuer != s.issuer {
		return "", fmt.Errorf("auth: %s is for issuer %q, not %q", well, doc.Issuer, s.issuer)
	}
	if err := checkHTTPURL(doc.JWKSURI); err != nil {
		return "", fmt.Errorf("auth: jwks_uri in %s: %w", well, err)
	}
	return doc.JWKSURI, nil
}

// fetchKeys reads the public signing keys at jwksURL. A key transponder cannot read, such
// as one of a type it does not know, is skipped instead of failing the set.
func (s *keySet) fetchKeys(ctx context.Context, jwksURL string) ([]jose.JSONWebKey, error) {
	var set struct {
		Keys []json.RawMessage `json:"keys"`
	}
	if err := s.getJSON(ctx, jwksURL, &set); err != nil {
		return nil, err
	}
	var keys []jose.JSONWebKey
	for _, raw := range set.Keys {
		var k jose.JSONWebKey
		if err := k.UnmarshalJSON(raw); err != nil {
			continue
		}
		if !k.IsPublic() || !k.Valid() || (k.Use != "" && k.Use != "sig") {
			continue
		}
		keys = append(keys, k)
	}
	if len(keys) == 0 {
		return nil, fmt.Errorf("auth: %s has no usable signing key", jwksURL)
	}
	return keys, nil
}

func (s *keySet) getJSON(ctx context.Context, rawURL string, into any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return fmt.Errorf("auth: %w", err)
	}
	req.Header.Set("Accept", "application/json")
	resp, err := s.client.Do(req)
	if err != nil {
		return fmt.Errorf("auth: fetching %s: %w", rawURL, err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("auth: fetching %s: status %d", rawURL, resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, jwksMaxBytes+1))
	if err != nil {
		return fmt.Errorf("auth: reading %s: %w", rawURL, err)
	}
	if len(body) > jwksMaxBytes {
		return fmt.Errorf("auth: %s is larger than %d bytes", rawURL, jwksMaxBytes)
	}
	if err := json.Unmarshal(body, into); err != nil {
		return fmt.Errorf("auth: parsing %s: %w", rawURL, err)
	}
	return nil
}

// checkHTTPURL reports whether s is an absolute http or https URL.
func checkHTTPURL(s string) error {
	u, err := url.Parse(s)
	if err != nil {
		return err
	}
	if (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return errors.New("must be an http or https URL")
	}
	return nil
}
