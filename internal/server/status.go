package server

import (
	"encoding/json"
	"net/http"
	"time"
)

func (s *Server) writeErr(w http.ResponseWriter, status int, errType, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]interface{}{
		"error": map[string]interface{}{
			"message": msg,
			"type":    errType,
			"param":   nil,
			"code":    errType,
		},
	})
}

func (s *Server) status(w http.ResponseWriter, r *http.Request) {
	states := s.Pool.Snapshot()
	now := time.Now()
	resp := map[string]interface{}{
		"version":  s.Version,
		"upstream": s.Client.Base,
		"now":      now,
	}
	keys := make([]map[string]interface{}, 0, len(states))
	var healthy, disabled, cooldown int
	var reqs, okc, tin, tout int64
	for _, st := range states {
		state := "healthy"
		switch {
		case st.Disabled:
			state = "disabled"
			disabled++
		case !st.CooldownUntil.IsZero() && now.Before(st.CooldownUntil):
			state = "cooldown"
			cooldown++
		default:
			healthy++
		}
		reqs += st.ReqCount
		okc += st.OkCount
		tin += st.TotalIn
		tout += st.TotalOut
		item := map[string]interface{}{
			"id":        maskID(st.ID),
			"label":     st.Label,
			"state":     state,
			"req_count": st.ReqCount,
			"ok_count":  st.OkCount,
			"err_count": st.ErrCount,
			"last_err":  st.LastErr,
		}
		if !st.CooldownUntil.IsZero() {
			item["cooldown_until"] = st.CooldownUntil
		}
		keys = append(keys, item)
	}
	resp["total_accounts"] = len(states)
	resp["healthy_accounts"] = healthy
	resp["disabled_accounts"] = disabled
	resp["cooldown_accounts"] = cooldown
	resp["total_requests"] = reqs
	resp["total_ok"] = okc
	resp["total_prompt_tokens"] = tin
	resp["total_completion_tokens"] = tout
	resp["accounts"] = keys
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(resp)
}

func maskID(id string) string {
	if len(id) <= 16 {
		return id
	}
	return id[:8] + "..." + id[len(id)-4:]
}
