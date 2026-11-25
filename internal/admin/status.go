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
	Cooldowns     []cooldownJSON   `json:"cooldowns"`
	OIDC          *oidcStatusJSON  `json:"oidc,omitempty"`
}

type oidcStatusJSON struct {
	Issuer      string  `json:"issuer"`
	Keys        int     `json:"keys"`
	FetchedAt   *string `json:"fetched_at,omitempty"`
	LastError   string  `json:"last_error,omitempty"`
	LastErrorAt *string `json:"last_error_at,omitempty"`
}

type cooldownJSON struct {
	Route    string `json:"route"`
	Provider string `json:"provider"`
	Model    string `json:"model"`
	Failures int    `json:"failures"`
	Until    string `json:"until"`
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
		Cooldowns:     []cooldownJSON{},
	}
	if h.d.Router != nil {
		if rt := h.d.Router(); rt != nil {
			for _, c := range rt.Cooldowns() {
				out.Cooldowns = append(out.Cooldowns, cooldownJSON{
					Route: c.Route, Provider: c.Provider, Model: c.Model,
					Failures: c.Failures, Until: formatTime(c.Until),
				})
			}
		}
	}
	if h.d.OIDC != nil {
		st := h.d.OIDC.KeyState()
		out.OIDC = &oidcStatusJSON{
			Issuer:      st.Issuer,
			Keys:        st.Keys,
			FetchedAt:   optTime(st.FetchedAt),
			LastError:   st.LastError,
			LastErrorAt: optTime(st.LastErrorAt),
		}
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
