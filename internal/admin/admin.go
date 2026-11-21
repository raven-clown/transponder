// Package admin serves the JSON admin API under /admin/v1/, as api/openapi.yaml describes it.
package admin

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/raven-clown/transponder/api"
	"github.com/raven-clown/transponder/internal/auth"
	"github.com/raven-clown/transponder/internal/config"
	"github.com/raven-clown/transponder/internal/runtime"
	"github.com/raven-clown/transponder/internal/secrets"
	"github.com/raven-clown/transponder/internal/store"
)

// Prefix is where the admin API is served.
const Prefix = "/admin/v1"

const maxBodyBytes = 1 << 20

// Store is what the admin API reads and writes.
type Store interface {
	store.KeyAdminStore
	store.UsageAdminStore
	store.ConfigStore
	store.AuditStore
	store.ConfigItemStore
	Ping(ctx context.Context) error
}

// Reloader swaps in a new runtime config once a write commits.
type Reloader interface {
	Check(ctx context.Context) (bool, error)
	Status() runtime.Status
}

// Deps are what the admin API needs.
type Deps struct {
	Auth   *auth.Authenticator
	Store  Store
	Reload Reloader
	Logger *slog.Logger
	Now    func() time.Time
	// Version is the transponder build, and StoreKind sqlite or postgres, for /status.
	Version   string
	StoreKind string
	Started   time.Time
	// File is the loaded config file. Its provider keys are used for the providers it
	// declares, and its other settings are kept when the runtime config is loaded.
	File *config.Bootstrap
	// Secrets seals provider keys. Nil means no secrets_key is set, and a provider key cannot
	// be stored.
	Secrets *secrets.Box
}

type handler struct {
	d   Deps
	mux *http.ServeMux
	// issuerSlashWarned keeps the trailing-slash hint from flooding the log.
	issuerSlashWarned sync.Once
}

// route is one endpoint. need is the role the caller must have.
type route struct {
	method, path string
	need         auth.Role
	fn           http.HandlerFunc
}

// New returns the handler for every path under Prefix.
func New(d Deps) http.Handler {
	h := &handler{d: d, mux: http.NewServeMux()}
	for _, rt := range h.routes() {
		mw := d.Auth.Middleware(rt.need, h.deny)
		h.mux.Handle(rt.method+" "+Prefix+rt.path, mw(rt.fn))
	}
	h.mux.HandleFunc("/", func(w http.ResponseWriter, _ *http.Request) {
		writeError(w, http.StatusNotFound, "not_found", "", "no such endpoint")
	})
	return h.mux
}

func (h *handler) routes() []route {
	read := func(path string, fn http.HandlerFunc) route {
		return route{http.MethodGet, path, auth.RoleViewer, fn}
	}
	write := func(method, path string, fn http.HandlerFunc) route {
		return route{method, path, auth.RoleAdmin, fn}
	}
	routes := []route{
		read("/openapi.yaml", h.openapi),
		read("/status", h.status),
		read("/keys", h.listKeys),
		write(http.MethodPost, "/keys", h.createKey),
		read("/keys/{name}", h.getKey),
		write(http.MethodPatch, "/keys/{name}", h.updateKey),
		write(http.MethodDelete, "/keys/{name}", h.deleteKey),
		write(http.MethodPost, "/keys/{name}/rotate", h.rotateKey),
		read("/usage", h.usage),
		read("/requests", h.requests),
		read("/audit", h.audit),
	}
	for _, s := range []*itemSpec{h.providerSpec(), h.routeSpec(), h.budgetSpec(), h.profileSpec()} {
		routes = append(routes, h.itemRoutes(s)...)
	}
	return routes
}

func (h *handler) openapi(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/yaml")
	_, _ = w.Write(api.OpenAPI)
}

func (h *handler) deny(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case errors.Is(err, auth.ErrUnauthenticated):
		h.d.Logger.DebugContext(r.Context(), "credentials refused", "reason", err.Error())
		if errors.Is(err, auth.ErrIssuerTrailingSlash) {
			h.issuerSlashWarned.Do(func() {
				h.d.Logger.WarnContext(r.Context(), "credentials refused", "reason", err.Error())
			})
		}
		writeError(w, http.StatusUnauthorized, "unauthenticated", "", "a valid API key or token is required")
	case errors.Is(err, auth.ErrForbidden):
		writeError(w, http.StatusForbidden, "forbidden", "", "your role does not allow this; reads need viewer and writes need admin")
	case errors.Is(err, auth.ErrUnavailable):
		h.d.Logger.WarnContext(r.Context(), "checking a token", "err", err)
		w.Header().Set("Retry-After", "10")
		writeError(w, http.StatusServiceUnavailable, "auth_unavailable", "", "the identity provider could not be reached to check the token")
	default:
		h.internal(w, r, "authenticating", err)
	}
}

// apiError is an error the handler answers with as it is.
type apiError struct {
	status      int
	code, field string
	message     string
}

func (e *apiError) Error() string { return e.message }

func invalid(field, format string, args ...any) *apiError {
	return &apiError{status: http.StatusBadRequest, code: "invalid", field: field, message: fmt.Sprintf(format, args...)}
}

// fail answers with err: an apiError as it is, a store error with its status, anything else
// as a logged 500.
func (h *handler) fail(w http.ResponseWriter, r *http.Request, what string, err error) {
	var ae *apiError
	switch {
	case errors.As(err, &ae):
		writeError(w, ae.status, ae.code, ae.field, ae.message)
	case errors.Is(err, store.ErrNotFound):
		writeError(w, http.StatusNotFound, "not_found", "", what+" not found")
	case errors.Is(err, store.ErrVersionConflict):
		writeError(w, http.StatusConflict, "version_conflict", "If-Match", what+" was changed by someone else; read it again and retry")
	case errors.Is(err, store.ErrConflict):
		writeError(w, http.StatusConflict, "already_exists", "name", what+" already exists")
	default:
		h.internal(w, r, what, err)
	}
}

func (h *handler) internal(w http.ResponseWriter, r *http.Request, what string, err error) {
	h.d.Logger.ErrorContext(r.Context(), "admin request failed", "what", what, "err", err, "request_id", r.Header.Get("X-Request-ID"))
	writeError(w, http.StatusInternalServerError, "internal", "", "the request could not be completed")
}

func writeError(w http.ResponseWriter, status int, code, field, message string) {
	body := map[string]any{"code": code, "message": message}
	if field != "" {
		body["field"] = field
	}
	writeJSON(w, status, map[string]any{"error": body})
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// writeVersioned writes a single resource with its version as the ETag.
func writeVersioned(w http.ResponseWriter, status int, version int64, v any) {
	w.Header().Set("ETag", strconv.Quote(strconv.FormatInt(version, 10)))
	writeJSON(w, status, v)
}

// decode reads a JSON body into dst. Unknown fields are refused, named in the error.
func decode(w http.ResponseWriter, r *http.Request, dst any) error {
	raw, err := readBody(w, r)
	if err != nil {
		return err
	}
	return decodeBytes(raw, dst)
}

func readBody(w http.ResponseWriter, r *http.Request) ([]byte, error) {
	raw, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxBodyBytes))
	var tooLarge *http.MaxBytesError
	switch {
	case errors.As(err, &tooLarge):
		return nil, &apiError{status: http.StatusRequestEntityTooLarge, code: "body_too_large", message: "the body is larger than 1 MiB"}
	case err != nil:
		return nil, invalid("", "could not read the body: %v", err)
	}
	return raw, nil
}

// decodeBytes reads one JSON object into dst, refusing unknown fields.
func decodeBytes(raw []byte, dst any) error {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	err := dec.Decode(dst)
	if err == nil {
		if dec.Decode(&struct{}{}) != io.EOF {
			return invalid("", "the body must hold one JSON object")
		}
		return nil
	}
	var typeErr *json.UnmarshalTypeError
	switch {
	case errors.As(err, &typeErr):
		return invalid(typeErr.Field, "%s must be a JSON %s", typeErr.Field, jsonType(typeErr.Type.Kind().String()))
	case strings.HasPrefix(err.Error(), "json: unknown field "):
		field := strings.Trim(strings.TrimPrefix(err.Error(), "json: unknown field "), `"`)
		return invalid(field, "unknown field %s", field)
	case errors.Is(err, io.EOF):
		return invalid("", "the body is empty; send a JSON object")
	}
	return invalid("", "the body is not valid JSON: %v", err)
}

func jsonType(kind string) string {
	switch kind {
	case "string":
		return "string"
	case "bool":
		return "boolean"
	case "slice", "array":
		return "array"
	case "map", "struct", "ptr":
		return "object"
	}
	return "number"
}

// ifMatch returns the version in If-Match, or zero when the header is absent.
func ifMatch(r *http.Request) (int64, error) {
	raw := strings.TrimSpace(r.Header.Get("If-Match"))
	if raw == "" {
		return 0, nil
	}
	v := strings.Trim(strings.TrimPrefix(raw, "W/"), `"`)
	n, err := strconv.ParseInt(v, 10, 64)
	if err != nil || n < 1 {
		return 0, invalid("If-Match", "If-Match must be a resource version such as \"3\", not %q", raw)
	}
	return n, nil
}

func formatTime(t time.Time) string { return t.UTC().Format(time.RFC3339Nano) }

// optTime is a time that is null when zero.
func optTime(t time.Time) *string {
	if t.IsZero() {
		return nil
	}
	s := formatTime(t)
	return &s
}
