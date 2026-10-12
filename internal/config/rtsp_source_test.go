package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRTSPSourcePrecedence(t *testing.T) {
	for _, source := range []string{"rtsp://receiver:8555/front", "rtsps://user:pass@receiver:322/front?channel=1", ""} {
		t.Run(source, func(t *testing.T) {
			dir := t.TempDir()
			data := "CAM_OPTIONS:\n  - CAM_NAME: Front Door\n    RTSP_SOURCE: rtsp://yaml-receiver/front\n"
			if err := os.WriteFile(filepath.Join(dir, "config.yml"), []byte(data), 0600); err != nil {
				t.Fatal(err)
			}
			t.Setenv("STATE_DIR", dir)
			t.Setenv("RTSP_SOURCE_FRONT_DOOR", source)
			cfg, err := Load()
			if err != nil {
				t.Fatal(err)
			}
			if got := cfg.CamRTSPSource("front_door"); got != source {
				t.Fatalf("source = %q, want env override %q", got, source)
			}
			if cfg.CamRTSPSource("other_camera") != "" {
				t.Fatal("override leaked to another camera")
			}
		})
	}
}

func TestYAMLRTSPSource(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "config.yml"), []byte("CAM_OPTIONS:\n  - CAM_NAME: garden\n    RTSP_SOURCE: rtsp://receiver/garden\n"), 0600); err != nil {
		t.Fatal(err)
	}
	cfg := &Config{StateDir: dir, CamOverrides: make(map[string]CamOverride)}
	if err := cfg.loadYAML(); err != nil {
		t.Fatal(err)
	}
	if cfg.CamRTSPSource("garden") != "rtsp://receiver/garden" {
		t.Fatal("YAML source was not loaded")
	}
}

func TestRejectInvalidRTSPSourceWithoutLeakingCredentials(t *testing.T) {
	for _, source := range []string{"exec:touch /tmp/example", "http://user:private-password@receiver/front", "rtsp://user:private-password@[broken/front", "rtsp:///front", "rtsp:front", "rtsp://receiver/front#video=h264"} {
		t.Run(source, func(t *testing.T) {
			t.Setenv("STATE_DIR", t.TempDir())
			t.Setenv("RTSP_SOURCE_FRONT", source)
			_, err := Load()
			if err == nil {
				t.Fatal("invalid source accepted")
			}
			if strings.Contains(err.Error(), "private-password") || strings.Contains(err.Error(), source) {
				t.Fatalf("configuration error exposed source: %s", err)
			}
		})
	}
}
