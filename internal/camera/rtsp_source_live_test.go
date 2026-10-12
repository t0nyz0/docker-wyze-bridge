package camera

import (
	"context"
	"io"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/IDisposable/docker-wyze-bridge/internal/config"
	"github.com/IDisposable/docker-wyze-bridge/internal/go2rtcmgr"
	"github.com/IDisposable/docker-wyze-bridge/internal/wyzeapi"
	"github.com/rs/zerolog"
)

// Run only against a disposable go2rtc instance with a reachable receiver.
func TestRTSPSourceLiveGo2RTC(t *testing.T) {
	base := os.Getenv("WYZE_BRIDGE_TEST_GO2RTC_URL")
	source := os.Getenv("WYZE_BRIDGE_TEST_RTSP_SOURCE")
	if base == "" || source == "" {
		t.Skip("set WYZE_BRIDGE_TEST_GO2RTC_URL and WYZE_BRIDGE_TEST_RTSP_SOURCE for an isolated live test")
	}
	const name = "lake_integration_test"
	api := go2rtcmgr.NewAPIClient(base, zerolog.Nop())
	t.Cleanup(func() { _ = api.DeleteStream(context.Background(), name) })
	cfg := &config.Config{CamOverrides: map[string]config.CamOverride{
		"LAKE_INTEGRATION_TEST": {RTSPSource: &source},
	}}
	m := NewManager(cfg, nil, api, zerolog.Nop())
	cam := NewCamera(wyzeapi.CameraInfo{Name: name, Model: "HL_PAN4"}, "hd", false, false)
	m.InjectCamera(name, cam)
	m.connectCamera(context.Background(), cam)
	consume := func() {
		t.Helper()
		client := &http.Client{Timeout: 20 * time.Second}
		resp, err := client.Get(strings.TrimRight(base, "/") + "/api/stream.mp4?src=" + name)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("live consumer status: %d", resp.StatusCode)
		}
		buf := make([]byte, 32768)
		if _, err := io.ReadFull(resp.Body, buf); err != nil {
			t.Fatalf("receiver did not deliver media: %v", err)
		}
		if !strings.Contains(string(buf[:128]), "ftyp") {
			t.Fatal("consumer did not receive an MP4 initialization segment")
		}
	}
	consume()
	if err := m.SetQuality(context.Background(), name, "sd"); err != nil {
		t.Fatal(err)
	}
	consume()
	m.RestartStream(context.Background(), name)
	consume()
	m.HealthCheck(context.Background())
	if cam.GetState() != StateStreaming {
		t.Fatal("healthy receiver was marked failed")
	}
}
