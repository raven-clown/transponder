package server

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"net/http"
)

type requestIDKey struct{}

// requestIDHeader is X-Request-ID in Go's canonical form, which Header.Get and Set use as it is
// instead of copying the key on every call.
const requestIDHeader = "X-Request-Id"

// RequestID keeps a caller's X-Request-ID when it is 1 to 128 printable ASCII
// characters, and generates one otherwise. The id is stored on the request
// context and set on the response before next runs, so the caller and the
// handler see the same value. A generated id is also written onto the request
// so usage recording and upstream forwarding read the id that was selected.
func RequestID(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := r.Header.Get(requestIDHeader)
		if !validRequestID(id) {
			id = newRequestID()
			r.Header.Set(requestIDHeader, id)
		}
		w.Header().Set(requestIDHeader, id)
		ctx := context.WithValue(r.Context(), requestIDKey{}, id)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// RequestIDFrom returns the id stored by RequestID, or "" when ctx has none.
func RequestIDFrom(ctx context.Context) string {
	id, _ := ctx.Value(requestIDKey{}).(string)
	return id
}

func validRequestID(id string) bool {
	if len(id) < 1 || len(id) > 128 {
		return false
	}
	for i := 0; i < len(id); i++ {
		if id[i] < ' ' || id[i] > '~' {
			return false
		}
	}
	return true
}

func newRequestID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		// The id has to be unique. Without the OS random source there is no safe value to return.
		panic(err)
	}
	return hex.EncodeToString(b[:])
}
