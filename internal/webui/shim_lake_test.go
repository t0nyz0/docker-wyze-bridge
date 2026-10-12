package webui

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/IDisposable/docker-wyze-bridge/internal/camera"
	"github.com/IDisposable/docker-wyze-bridge/internal/config"
	"github.com/IDisposable/docker-wyze-bridge/internal/wyzeapi"
)

type testLakeProvider struct {
	calls int
	fail  bool
}

func (p *testLakeProvider) RenewLakeToken(_ context.Context, _, _ string, _ int) (string, error) {
	p.calls++
	if p.fail {
		return "", errors.New("private-session-secret")
	}
	return "test-renewed-token", nil
}

func (p *testLakeProvider) GetLakeSession(_ context.Context, mac, model string) (*wyzeapi.LakeSession, error) {
	p.calls++
	if mac != "test-camera" || model != "HL_PAN4" {
		return nil, errors.New("wrong camera")
	}
	if p.fail {
		return nil, errors.New("private-session-secret")
	}
	return &wyzeapi.LakeSession{RTCToken: "test-token"}, nil
}

func TestLakeShimAccessBoundary(t *testing.T) {
	for _, tc := range []struct {
		name, remote, method, model string
		configured, fail            bool
		status                      int
	}{
		{"loopback", "127.0.0.1:1234", "POST", "HL_PAN4", true, false, 200},
		{"ipv6", "[::1]:1234", "POST", "HL_PAN4", true, false, 200},
		{"lan", "10.0.0.99:1234", "POST", "HL_PAN4", true, false, 404},
		{"get", "127.0.0.1:1234", "GET", "HL_PAN4", true, false, 405},
		{"not-configured", "127.0.0.1:1234", "POST", "HL_PAN4", false, false, 404},
		{"other-model", "127.0.0.1:1234", "POST", "HL_CAM4", true, false, 404},
		{"upstream-error", "127.0.0.1:1234", "POST", "HL_PAN4", true, true, 502},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, _ := testServer(t)
			input := "rtsp://receiver/front"
			if tc.configured {
				s.cfg.CamOverrides["FRONT"] = config.CamOverride{RTSPSource: &input}
			}
			s.camMgr.InjectCamera("front", camera.NewCamera(wyzeapi.CameraInfo{Name: "front", Model: tc.model, MAC: "test-camera"}, "hd", false, false))
			p := &testLakeProvider{fail: tc.fail}
			s.lake = p
			mux := http.NewServeMux()
			s.registerRoutes(mux)
			r := httptest.NewRequest(tc.method, "/internal/wyze/lake/front", nil)
			r.RemoteAddr = tc.remote
			r.Header.Set("X-Forwarded-For", "127.0.0.1")
			w := httptest.NewRecorder()
			mux.ServeHTTP(w, r)
			if w.Code != tc.status {
				t.Fatalf("status %d, want %d", w.Code, tc.status)
			}
			if strings.Contains(w.Body.String(), "private-session-secret") {
				t.Fatal("session error leaked")
			}
			if tc.status == 200 && w.Header().Get("Cache-Control") != "no-store" {
				t.Fatal("session response can be cached")
			}
			if tc.status != 200 && tc.status != 502 && p.calls != 0 {
				t.Fatal("blocked request reached provisioning")
			}
		})
	}
}

func TestLakeRenewalShim(t *testing.T) {
	s, _ := testServer(t)
	input := "rtsp://receiver/front"
	s.cfg.CamOverrides["FRONT"] = config.CamOverride{RTSPSource: &input}
	s.camMgr.InjectCamera("front", camera.NewCamera(wyzeapi.CameraInfo{Name: "front", Model: "HL_PAN4", MAC: "test-camera"}, "hd", false, false))
	p := &testLakeProvider{}
	s.lake = p
	mux := http.NewServeMux()
	s.registerRoutes(mux)
	for _, body := range []string{`{"uid":12345}`, `{"uid":0}`, `{"uid":"secret"}`, `bad-json`} {
		r := httptest.NewRequest("POST", "/internal/wyze/lake/front/renew", strings.NewReader(body))
		r.RemoteAddr = "127.0.0.1:1234"
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, r)
		if body == `{"uid":12345}` {
			if w.Code != 200 || !strings.Contains(w.Body.String(), "test-renewed-token") {
				t.Fatal("renewal failed")
			}
		} else if w.Code != 400 {
			t.Fatal("bad renewal input accepted")
		}
	}
	if p.calls != 1 {
		t.Fatal("invalid renewal reached provider")
	}
}
