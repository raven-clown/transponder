package admin

import (
	"encoding/base64"
	"net/http"
	"net/url"
	"strconv"
	"time"
)

const (
	defaultLimit = 50
	maxLimit     = 500
)

// page is the cursor and limit of a list request. The cursor is the sort key of the last item
// of the previous page, base64url encoded so callers treat it as opaque.
type page struct {
	limit   int
	after   string
	afterID int64
}

func readPage(r *http.Request) (page, error) {
	q := r.URL.Query()
	p := page{limit: defaultLimit}
	if s := q.Get("limit"); s != "" {
		n, err := strconv.Atoi(s)
		if err != nil || n < 1 || n > maxLimit {
			return p, invalid("limit", "limit must be a whole number from 1 to %d", maxLimit)
		}
		p.limit = n
	}
	if s := q.Get("cursor"); s != "" {
		raw, err := base64.RawURLEncoding.DecodeString(s)
		if err != nil || len(raw) == 0 {
			return p, invalid("cursor", "cursor is not one this API returned")
		}
		p.after = string(raw)
		if id, err := strconv.ParseInt(p.after, 10, 64); err == nil && id > 0 {
			p.afterID = id
		}
	}
	return p, nil
}

func cursor(key string) string { return base64.RawURLEncoding.EncodeToString([]byte(key)) }

type listJSON[T any] struct {
	Items      []T    `json:"items"`
	NextCursor string `json:"next_cursor,omitempty"`
}

// writeIDPage writes items fetched newest first with one more than the limit, so a next page
// exists when that extra one came back.
func writeIDPage[T any](w http.ResponseWriter, items []T, limit int, id func(T) int64) {
	out := listJSON[T]{Items: items}
	if len(items) > limit {
		out.Items = items[:limit]
		out.NextCursor = cursor(strconv.FormatInt(id(items[limit-1]), 10))
	}
	writeJSON(w, http.StatusOK, out)
}

// keyPage returns the page of items, sorted by key, that comes after p's cursor, and the
// cursor of the next page.
func keyPage[T any](items []T, p page, key func(T) string) listJSON[T] {
	start := 0
	if p.after != "" {
		for start < len(items) && key(items[start]) <= p.after {
			start++
		}
	}
	out := listJSON[T]{Items: items[start:]}
	if len(out.Items) > p.limit {
		out.Items = out.Items[:p.limit]
		out.NextCursor = cursor(key(out.Items[p.limit-1]))
	}
	if out.Items == nil {
		out.Items = []T{}
	}
	return out
}

// readRange reads from and to. When now is set, a missing from is the start of now's month and
// a missing to is now; otherwise missing ends stay zero, which means open.
func readRange(q url.Values, now time.Time) (from, to time.Time, err error) {
	if from, err = readTime(q, "from"); err != nil {
		return
	}
	if to, err = readTime(q, "to"); err != nil {
		return
	}
	if !now.IsZero() {
		n := now.UTC()
		if from.IsZero() {
			from = time.Date(n.Year(), n.Month(), 1, 0, 0, 0, 0, time.UTC)
		}
		if to.IsZero() {
			to = n
		}
	}
	if !from.IsZero() && !to.IsZero() && !to.After(from) {
		return from, to, invalid("to", "to must be after from")
	}
	return from, to, nil
}

func readTime(q url.Values, name string) (time.Time, error) {
	s := q.Get(name)
	if s == "" {
		return time.Time{}, nil
	}
	if t, err := time.Parse(time.RFC3339Nano, s); err == nil {
		return t, nil
	}
	if t, err := time.Parse(time.DateOnly, s); err == nil {
		return t, nil
	}
	return time.Time{}, invalid(name, "%s must be an RFC 3339 time or a date such as 2026-11-01", name)
}
