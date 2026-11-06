package server

import (
	"encoding/json"
	"net/http"
)

type wireModel struct {
	ID      string `json:"id"`
	Object  string `json:"object"`
	Created int64  `json:"created"`
	OwnedBy string `json:"owned_by"`
}

// models lists the route names a caller can use, without asking any provider.
func (g *gateway) models(w http.ResponseWriter, r *http.Request) {
	if c := g.begin(w, r, "models"); c == nil {
		return
	}
	names := g.d.Router.Models()
	data := make([]wireModel, len(names))
	for i, n := range names {
		data[i] = wireModel{ID: n, Object: "model", OwnedBy: "transponder"}
	}
	body, err := json.Marshal(struct {
		Object string      `json:"object"`
		Data   []wireModel `json:"data"`
	}{"list", data})
	if err != nil {
		writeError(w, http.StatusInternalServerError, "api_error", "", "", "could not encode the model list")
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write(body) //nolint:gosec // JSON we encoded, served as application/json
}
