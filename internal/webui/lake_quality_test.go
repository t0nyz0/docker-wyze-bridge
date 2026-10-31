package webui

import (
	"context"
	"testing"

	"github.com/IDisposable/docker-wyze-bridge/internal/config"
	"github.com/IDisposable/docker-wyze-bridge/internal/wyzeapi"
	"github.com/rs/zerolog"
)

type unusedQualityProvider struct{}

func (unusedQualityProvider) GetLakeSession(context.Context, string, string) (*wyzeapi.LakeSession, error) {
	panic("quality wiring must not provision camera sessions")
}
func (unusedQualityProvider) RenewLakeToken(context.Context, string, string, int) (string, error) {
	panic("quality wiring must not renew camera tokens")
}

func TestQualityProbeOnlyUsesEmbeddedEngine(t *testing.T) {
	for _, external := range []string{"", "http://external-engine.invalid:1984"} {
		s := NewServer(Options{Config: &config.Config{Go2RTCRTSPPort: 8554, Go2RTCURL: external}, Lake: unusedQualityProvider{}, Log: zerolog.Nop()})
		if (s.lakeBroker.ProbeQuality != nil) != (external == "") {
			t.Fatal("quality probe did not respect embedded-engine ownership")
		}
		s.lakeBroker.Close()
	}
}
