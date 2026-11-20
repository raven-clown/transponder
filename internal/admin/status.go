package admin

import (
	"net/http"
)

type statusJSON struct {
	Version       string           `json:"version"`
	StartedAt     string           `json:"started_at"`
	UptimeSeconds int64            `json:"uptime_seconds"`
	Store         storeStatusJSON  `json:"store"`
	Config        configStatusJSON `json:"config"`
}

type storeStatusJSON struct {
	Kind    string `json:"kind"`
	Healthy bool   `json:"healthy"`
	Error   string `json:"error,omitempty"`
}

type configStatusJSON struct {
	Version       int64   `json:"version"`
	LoadedAt      string  `json:"loaded_at"`
	ReloadFailed  bool    `json:"reload_failed"`
	FailedVersion int64   `json:"failed_version,omitempty"`
	Error         string  `json:"error,omitempty"`
	FailedAt      *string `json:"failed_at,omitempty"`
}

func (h *handler) status(w http.ResponseWriter, r *http.Request) {
	now := h.d.Now()
	out := statusJSON{
		Version:       h.d.Version,
		StartedAt:     formatTime(h.d.Started),
		UptimeSeconds: int64(now.Sub(h.d.Started).Seconds()),
		Store:         storeStatusJSON{Kind: h.d.StoreKind, Healthy: true},
	}
	if err := h.d.Store.Ping(r.Context()); err != nil {
		out.Store.Healthy, out.Store.Error = false, err.Error()
	}
	if h.d.Reload != nil {
		st := h.d.Reload.Status()
		out.Config = configStatusJSON{
			Version:       st.Version,
			LoadedAt:      formatTime(st.LoadedAt),
			ReloadFailed:  st.Failed,
			FailedVersion: st.FailedVersion,
			Error:         st.Error,
		}
		if st.Failed {
			out.Config.FailedAt = optTime(st.FailedAt)
		}
	}
	writeJSON(w, http.StatusOK, out)
}
