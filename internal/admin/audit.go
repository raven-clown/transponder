package admin

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"time"

	"github.com/raven-clown/transponder/internal/auth"
	"github.com/raven-clown/transponder/internal/store"
)

// Audit actions.
const (
	actionCreate = "create"
	actionUpdate = "update"
	actionDelete = "delete"
	actionRotate = "rotate"
)

// change is one write: what it is for the audit log, and the store calls that make it.
type change struct {
	kind, resource, action string
	// apply writes with ctx, which joins the transaction, and returns the resource before and
	// after in its API shape, nil for none. Secrets must already be left out.
	apply func(ctx context.Context) (before, after any, err error)
}

// commit runs c in one config transaction with its audit record, so both commit or neither
// does, and the config version goes up with them. Then it reloads, so on this replica the
// change is live before the handler answers.
func (h *handler) commit(r *http.Request, c change) error {
	id, _ := auth.FromContext(r.Context())
	_, err := h.d.Store.UpdateConfig(r.Context(), func(ctx context.Context) error {
		before, after, err := c.apply(ctx)
		if err != nil {
			return err
		}
		diff, err := json.Marshal(map[string]any{"before": before, "after": after})
		if err != nil {
			return err
		}
		return h.d.Store.AddAudit(ctx, store.AuditRecord{
			OrgID:     orgOf(id),
			Time:      h.d.Now(),
			Actor:     id.Subject(),
			Action:    c.action,
			Kind:      c.kind,
			Resource:  c.resource,
			Diff:      diff,
			RequestID: r.Header.Get("X-Request-ID"),
		})
	})
	if err != nil {
		return err
	}
	h.reload(r)
	return nil
}

// reload swaps in the committed config. A config that fails to load keeps the old one serving
// and shows in /status; the write itself has committed, so it is not an error here.
func (h *handler) reload(r *http.Request) {
	if h.d.Reload == nil {
		return
	}
	// The write has committed, so the reload runs even when the caller has gone.
	if _, err := h.d.Reload.Check(context.WithoutCancel(r.Context())); err != nil {
		h.d.Logger.WarnContext(r.Context(), "reloading after an admin write", "err", err)
	}
}

func orgOf(id auth.Identity) string {
	if id.OrgID == "" {
		return store.DefaultOrg
	}
	return id.OrgID
}

type auditJSON struct {
	ID        int64           `json:"id"`
	Time      string          `json:"time"`
	Actor     string          `json:"actor"`
	Action    string          `json:"action"`
	Kind      string          `json:"kind"`
	Resource  string          `json:"resource"`
	Diff      json.RawMessage `json:"diff"`
	RequestID string          `json:"request_id"`
}

func (h *handler) audit(w http.ResponseWriter, r *http.Request) {
	p, err := readPage(r)
	if err != nil {
		h.fail(w, r, "audit", err)
		return
	}
	q := r.URL.Query()
	from, to, err := readRange(q, time.Time{})
	if err != nil {
		h.fail(w, r, "audit", err)
		return
	}
	action := q.Get("action")
	if action != "" && action != actionCreate && action != actionUpdate && action != actionDelete && action != actionRotate {
		h.fail(w, r, "audit", invalid("action", "action must be create, update, delete or rotate"))
		return
	}
	id, _ := auth.FromContext(r.Context())
	kind, resource, _ := strings.Cut(q.Get("resource"), "/")
	recs, err := h.d.Store.ListAudit(r.Context(), store.AuditQuery{
		OrgID:    orgOf(id),
		Actor:    q.Get("actor"),
		Action:   action,
		Kind:     kind,
		Resource: resource,
		From:     from,
		To:       to,
		BeforeID: p.afterID,
		Limit:    p.limit + 1,
	})
	if err != nil {
		h.fail(w, r, "audit", err)
		return
	}
	items := make([]auditJSON, 0, len(recs))
	for _, rec := range recs {
		items = append(items, auditJSON{
			ID:        rec.ID,
			Time:      formatTime(rec.Time),
			Actor:     rec.Actor,
			Action:    rec.Action,
			Kind:      rec.Kind,
			Resource:  rec.Resource,
			Diff:      json.RawMessage(rec.Diff),
			RequestID: rec.RequestID,
		})
	}
	writeIDPage(w, items, p.limit, func(a auditJSON) int64 { return a.ID })
}
