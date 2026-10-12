package camera

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/IDisposable/docker-wyze-bridge/internal/config"
	"github.com/IDisposable/docker-wyze-bridge/internal/go2rtcmgr"
	"github.com/IDisposable/docker-wyze-bridge/internal/wyzeapi"
	"github.com/rs/zerolog"
)

func TestRTSPSourceSurvivesLifecycle(t *testing.T) {
	source := "rtsp://receiver:8555/front"
	var registered []string
	deletions := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "PUT" {
			registered = append(registered, r.URL.Query().Get("src"))
		}
		if r.Method == "DELETE" {
			deletions++
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()
	cfg := &config.Config{CamOverrides: map[string]config.CamOverride{"FRONT": {RTSPSource: &source}}, BridgePort: 5080}
	m := NewManager(cfg, nil, go2rtcmgr.NewAPIClient(srv.URL, zerolog.Nop()), zerolog.Nop())
	cam := NewCamera(wyzeapi.CameraInfo{Name: "front", Model: "HL_PAN4"}, "hd", false, false)
	cam.SetForceWebRTC(true)
	m.InjectCamera("front", cam)
	m.connectCamera(context.Background(), cam)
	beforeQualityDeletes := deletions
	if err := m.SetQuality(context.Background(), "front", "sd"); err != nil {
		t.Fatal(err)
	}
	if deletions != beforeQualityDeletes || len(registered) != 1 {
		t.Fatal("quality control interrupted a receiver-owned input")
	}
	m.RestartStream(context.Background(), "front")
	if len(registered) != 2 {
		t.Fatalf("registrations = %d, want connect and explicit restart", len(registered))
	}
	for _, got := range registered {
		if got != source {
			t.Fatalf("source replaced by model route: %q", got)
		}
	}
	if cam.TUTKFailStreak() != 0 {
		t.Fatal("external receiver triggered TUTK fallback")
	}
}

func TestRTSPHealthIdleAndMissingStream(t *testing.T) {
	for _, tc := range []struct {
		name    string
		streams map[string]*go2rtcmgr.StreamInfo
		want    State
	}{
		{"idle", map[string]*go2rtcmgr.StreamInfo{"front": {}}, StateStreaming},
		{"missing", map[string]*go2rtcmgr.StreamInfo{}, StateError},
		{"null", map[string]*go2rtcmgr.StreamInfo{"front": nil}, StateError},
		{"consumer_without_producer", map[string]*go2rtcmgr.StreamInfo{"front": {Consumers: []go2rtcmgr.ConsumerInfo{{URL: "rtsp://viewer"}}}}, StateError},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { json.NewEncoder(w).Encode(tc.streams) }))
			defer srv.Close()
			source := "rtsp://receiver/front"
			cfg := &config.Config{CamOverrides: map[string]config.CamOverride{"FRONT": {RTSPSource: &source}}}
			m := NewManager(cfg, nil, go2rtcmgr.NewAPIClient(srv.URL, zerolog.Nop()), zerolog.Nop())
			cam := NewCamera(wyzeapi.CameraInfo{Name: "front", Model: "HL_PAN4"}, "hd", false, false)
			cam.SetState(StateStreaming)
			m.InjectCamera("front", cam)
			m.HealthCheck(context.Background())
			if cam.GetState() != tc.want {
				t.Fatalf("state = %s, want %s", cam.GetState(), tc.want)
			}
		})
	}
}

func TestRTSPRegistrationErrorsDoNotExposeSource(t *testing.T) {
	source := "rtsp://user:private-password@receiver/front?token=private-token"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "PUT" {
			http.Error(w, source, http.StatusBadRequest)
		} else {
			w.WriteHeader(http.StatusOK)
		}
	}))
	defer srv.Close()
	var logs bytes.Buffer
	cfg := &config.Config{CamOverrides: map[string]config.CamOverride{"FRONT": {RTSPSource: &source}}}
	m := NewManager(cfg, nil, go2rtcmgr.NewAPIClient(srv.URL, zerolog.New(&logs)), zerolog.New(&logs))
	cam := NewCamera(wyzeapi.CameraInfo{Name: "front", Model: "HL_PAN4"}, "hd", false, false)
	m.InjectCamera("front", cam)
	m.connectCamera(context.Background(), cam)
	err := m.SetQuality(context.Background(), "front", "sd")
	if err != nil {
		t.Fatal("receiver-owned quality control should not register a source")
	}
	for _, text := range []string{logs.String()} {
		if strings.Contains(text, "private-password") || strings.Contains(text, "private-token") {
			t.Fatal("source credentials leaked")
		}
	}
}

func TestDefaultGwellHealthBehaviorIsPreserved(t *testing.T) {
	// The receiver feature must not enable producer-count checks for existing
	// Gwell/WebRTC cameras: upstream exempts all Gwell models from these checks.
	for _, model := range []string{"GW_WC", "GW_BE1", "GW_DBD"} {
		for _, external := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/external=%t", model, external), func(t *testing.T) {
				srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					_, _ = w.Write([]byte(`{}`))
				}))
				defer srv.Close()
				cfg := &config.Config{CamOverrides: make(map[string]config.CamOverride)}
				if external {
					source := "rtsp://receiver/front"
					cfg.CamOverrides["FRONT"] = config.CamOverride{RTSPSource: &source}
				}
				m := NewManager(cfg, nil, go2rtcmgr.NewAPIClient(srv.URL, zerolog.Nop()), zerolog.Nop())
				cam := NewCamera(wyzeapi.CameraInfo{Name: "front", Model: model}, "hd", false, false)
				if !cam.GetInfo().IsGwell() {
					t.Fatal("fixture must be a Gwell model")
				}
				cam.SetState(StateStreaming)
				m.InjectCamera("front", cam)
				m.HealthCheck(context.Background())
				want := StateStreaming
				if external {
					want = StateError // a missing explicitly configured input needs repair
				}
				if cam.GetState() != want {
					t.Fatalf("state = %s, want %s", cam.GetState(), want)
				}
			})
		}
	}
}
