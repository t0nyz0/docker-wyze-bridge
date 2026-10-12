package webui

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"

	"github.com/IDisposable/docker-wyze-bridge/internal/wyzeapi"
)

type LakeSessionProvider interface {
	GetLakeSession(context.Context, string, string) (*wyzeapi.LakeSession, error)
	RenewLakeToken(context.Context, string, string, int) (string, error)
}

// Only a loopback companion can request a session, and only for an explicitly
// configured Pan v4 receiver input. Forwarded headers never authorize access.
func (s *Server) handleShimLakeSession(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		http.Error(w, "POST required", http.StatusMethodNotAllowed)
		return
	}
	name := strings.TrimPrefix(r.URL.Path, "/internal/wyze/lake/")
	renew := strings.HasSuffix(name, "/renew")
	if renew {
		name = strings.TrimSuffix(name, "/renew")
	}
	if name == "" || strings.Contains(name, "/") {
		http.NotFound(w, r)
		return
	}
	cam := s.camMgr.GetCamera(name)
	if cam == nil || cam.GetInfo().Model != "HL_PAN4" || s.cfg.CamRTSPSource(name) == "" {
		http.NotFound(w, r)
		return
	}
	if s.lake == nil {
		http.Error(w, "Lake provider unavailable", http.StatusServiceUnavailable)
		return
	}
	info := cam.GetInfo()
	if renew {
		var body struct {
			UID int `json:"uid"`
		}
		if err := json.NewDecoder(io.LimitReader(r.Body, 1024)).Decode(&body); err != nil || body.UID < 10000 || body.UID > 99999 {
			http.Error(w, "valid session UID required", http.StatusBadRequest)
			return
		}
		token, err := s.lake.RenewLakeToken(r.Context(), info.MAC, info.Model, body.UID)
		if err != nil {
			http.Error(w, "Lake token renewal failed", http.StatusBadGateway)
			return
		}
		writeJSON(w, map[string]string{"rtc_token": token})
		return
	}
	session, err := s.lake.GetLakeSession(r.Context(), info.MAC, info.Model)
	if err != nil {
		s.log.Warn().Str("cam", name).Msg("Lake provisioning failed")
		http.Error(w, "Lake provisioning failed; check Bridge authentication and camera availability", http.StatusBadGateway)
		return
	}
	writeJSON(w, session)
}
