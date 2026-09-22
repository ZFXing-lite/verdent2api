package server

import (
	"encoding/json"
	"net/http"
	"time"

	"verdent2api/internal/pool"
)

// statusResp /status 响应体。
type statusResp struct {
	Version    string    `json:"version"`
	Upstream   string    `json:"upstream"`
	Now        time.Time `json:"now"`
	Total      int       `json:"total_keys"`
	Healthy    int       `json:"healthy_keys"`
	Disabled   int       `json:"disabled_keys"`
	Cooldown   int       `json:"cooldown_keys"`
	InFlight   int       `json:"in_flight"`
	TotalReqs  int64     `json:"total_requests"`
	TotalOK    int64     `json:"total_ok"`
	TotalInTok int64     `json:"total_prompt_tokens"`
	TotalOutTk int64     `json:"total_completion_tokens"`
	Keys       []keyInfo `json:"keys"`
}

type keyInfo struct {
	Label          string     `json:"label"`
	Key            string     `json:"key"`
	State          string     `json:"state"`
	CooldownUntil  *time.Time `json:"cooldown_until,omitempty"`
	DisabledReason string     `json:"disabled_reason,omitempty"`
	ReqCount       int64      `json:"req_count"`
	OkCount        int64      `json:"ok_count"`
	ErrCount       int        `json:"err_count"`
	InFlight       int        `json:"in_flight"`
	LastUsed       *time.Time `json:"last_used,omitempty"`
	LastErr        string     `json:"last_err,omitempty"`
}

// status 输出账号池快照（key 脱敏）。
func (s *Server) status(w http.ResponseWriter, r *http.Request) {
	states := s.Pool.Snapshot()
	resp := statusResp{
		Version:  s.Version,
		Upstream: s.Client.BaseURL,
		Now:      time.Now(),
		Keys:     make([]keyInfo, 0, len(states)),
	}
	now := time.Now()
	for _, st := range states {
		state := "healthy"
		switch {
		case st.Disabled:
			state = "disabled"
			resp.Disabled++
		case !st.CooldownUntil.IsZero() && now.Before(st.CooldownUntil):
			state = "cooldown"
			resp.Cooldown++
		default:
			resp.Healthy++
		}
		resp.Total++
		resp.TotalReqs += st.ReqCount
		resp.TotalOK += st.OkCount
		resp.TotalInTok += st.TotalIn
		resp.TotalOutTk += st.TotalOut
		resp.InFlight += inflightOf(st.APIKey)

		ki := keyInfo{
			Label:          st.Label,
			Key:            maskKey(st.APIKey),
			State:          state,
			DisabledReason: st.DisabledReason,
			ReqCount:       st.ReqCount,
			OkCount:        st.OkCount,
			ErrCount:       st.ErrCount,
			InFlight:       inflightOf(st.APIKey),
			LastErr:        st.LastErr,
		}
		if !st.CooldownUntil.IsZero() {
			t := st.CooldownUntil
			ki.CooldownUntil = &t
		}
		if !st.LastUsed.IsZero() {
			t := st.LastUsed
			ki.LastUsed = &t
		}
		resp.Keys = append(resp.Keys, ki)
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(resp)
}

func inflightOf(apiKey string) int {
	return pool.InFlightCount(apiKey)
}

func maskKey(k string) string {
	if len(k) <= 12 {
		return "***"
	}
	return k[:6] + "..." + k[len(k)-4:]
}
