package webui

import (
	"encoding/json"
	"net/http/httptest"
	"testing"

	"github.com/IDisposable/docker-wyze-bridge/internal/camera"
	"github.com/IDisposable/docker-wyze-bridge/internal/config"
	"github.com/IDisposable/docker-wyze-bridge/internal/wyzeapi"
)

func TestRTSPInputMetricsAndGwellExclusion(t *testing.T) {
	srv, _ := testServer(t)
	source := "rtsp://receiver/window"
	srv.cfg.CamOverrides = map[string]config.CamOverride{"WINDOW": {RTSPSource: &source}}
	cam := camera.NewCamera(wyzeapi.CameraInfo{Name: "window", Model: "GW_WC", MAC: "AABB11223344"}, "hd", false, false)
	cam.SetForceWebRTC(true)
	srv.camMgr.InjectCamera("window", cam)
	m := srv.buildMetricsSnapshot()
	if len(m.Cameras) != 1 || m.Cameras[0].Protocol != "rtsp" || m.Cameras[0].ProtocolForced {
		t.Fatal("metrics did not reflect the external receiver")
	}
	w := httptest.NewRecorder()
	srv.handleShimCameraList(w, newShimReq("GET", "/internal/wyze/Camera/CameraList"))
	var ids []string
	if err := json.Unmarshal(w.Body.Bytes(), &ids); err != nil {
		t.Fatal(err)
	}
	if len(ids) != 0 {
		t.Fatal("gwell-proxy can compete with the external receiver")
	}
}
