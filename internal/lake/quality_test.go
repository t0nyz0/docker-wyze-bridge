package lake

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/IDisposable/docker-wyze-bridge/internal/wyzeapi"
	"github.com/gorilla/websocket"
)

func resolutionFixture(dimensions func(int) (int, int)) []byte {
	frames := make([]decodedFrame, 240)
	for i := range frames {
		w, h := dimensions(i)
		frames[i] = decodedFrame{w, h, fmt.Sprintf("%.3f", 100+float64(i)/20)}
	}
	raw, _ := json.Marshal(struct {
		Frames []decodedFrame `json:"frames"`
	}{frames})
	return raw
}

func TestDecodedQualityRegression(t *testing.T) {
	for _, tc := range []struct {
		name       string
		dimensions func(int) (int, int)
		wantLow    bool
	}{
		{"native-4k", func(int) (int, int) { return 3840, 2160 }, false},
		{"valid-720p", func(int) (int, int) { return 1280, 720 }, false},
		{"persistent-360p", func(int) (int, int) { return 640, 360 }, true},
		{"4k-spikes-in-low-video", func(i int) (int, int) {
			if i%20 == 0 {
				return 3840, 2160
			}
			return 640, 360
		}, true},
		{"first-frame-4k-rest-low", func(i int) (int, int) {
			if i == 0 {
				return 3840, 2160
			}
			return 640, 360
		}, true},
		{"observed-4k-lead-then-360p", func(i int) (int, int) {
			if i < 29 {
				return 3840, 2160
			}
			return 640, 360
		}, true},
		{"cached-low-startup-only", func(i int) (int, int) {
			if i < 60 {
				return 640, 360
			}
			return 3840, 2160
		}, false},
		{"quality-recovered-at-tail", func(i int) (int, int) {
			if i < 190 {
				return 640, 360
			}
			return 3840, 2160
		}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			sample, err := parseResolutionSample(resolutionFixture(tc.dimensions))
			if err != nil || sample.Frames != 180 || sample.low() != tc.wantLow {
				t.Fatalf("wrong decoded classification: %+v error=%v", sample, err)
			}
		})
	}
}

func TestUnknownDecodedQualityDoesNotLookHealthy(t *testing.T) {
	for _, raw := range [][]byte{
		[]byte(`{`), []byte(`{"frames":[]}`),
		[]byte(`{"frames":[{"width":640,"height":360,"best_effort_timestamp_time":"NaN"}]}`),
		[]byte(`{"frames":[{"width":640,"height":360,"best_effort_timestamp_time":"+Inf"}]}`),
		[]byte(`{"frames":[{"width":640,"height":360,"best_effort_timestamp_time":"N/A"}]}`),
		[]byte(`{"frames":[{"width":640,"height":360,"best_effort_timestamp_time":"0"},{"width":640,"height":360,"best_effort_timestamp_time":"-1"}]}`),
		make([]byte, qualityOutputLimit+1),
	} {
		if _, err := parseResolutionSample(raw); err == nil {
			t.Fatal("accepted incomplete/invalid probe")
		}
	}
}

func lowQualitySample() ResolutionSample {
	return ResolutionSample{Width: 640, Height: 360, Frames: 180, LowFrames: 170, TailFrames: 41, TailLowFrames: 39}
}

func TestQualityPolicyRequiresConsecutiveEvidence(t *testing.T) {
	start := time.Unix(10000, 0)
	for _, interruption := range []struct {
		sample ResolutionSample
		err    error
	}{
		{ResolutionSample{Frames: 180, TailFrames: 41}, nil},
		{ResolutionSample{}, errors.New("probe unavailable")},
	} {
		var p qualityPolicy
		if p.observe(start, lowQualitySample(), nil) {
			t.Fatal("one sample requested quality")
		}
		if p.observe(start.Add(time.Minute), interruption.sample, interruption.err) {
			t.Fatal("unknown/healthy sample requested quality")
		}
		if p.observe(start.Add(2*time.Minute), lowQualitySample(), nil) {
			t.Fatal("streak survived a healthy/unknown sample")
		}
		if !p.observe(start.Add(3*time.Minute), lowQualitySample(), nil) {
			t.Fatal("two sustained low samples did not request quality")
		}
	}
}

func TestQualityPolicyBreaksFastAndStaleStreaks(t *testing.T) {
	for _, gap := range []time.Duration{time.Second, 4 * time.Minute, -time.Minute} {
		var p qualityPolicy
		now := time.Unix(10000, 0)
		p.observe(now, lowQualitySample(), nil)
		if p.observe(now.Add(gap), lowQualitySample(), nil) {
			t.Fatalf("invalid interval continued low streak: %s", gap)
		}
	}
}

func TestQualityPolicyCooldownAndRollingLimit(t *testing.T) {
	var p qualityPolicy
	start := time.Unix(10000, 0)
	requestAt := func(now time.Time) bool {
		p.observe(now.Add(-time.Minute), lowQualitySample(), nil)
		return p.observe(now, lowQualitySample(), nil)
	}
	if !requestAt(start) {
		t.Fatal("initial confirmed low was not recovered")
	}
	if requestAt(start.Add(2 * time.Minute)) {
		t.Fatal("cooldown allowed repeated controls")
	}
	if !requestAt(start.Add(15*time.Minute)) || !requestAt(start.Add(30*time.Minute)) {
		t.Fatal("eligible bounded recovery was suppressed")
	}
	if requestAt(start.Add(45 * time.Minute)) {
		t.Fatal("more than three requests in one hour")
	}
	if !requestAt(start.Add(time.Hour)) {
		t.Fatal("expired hourly history never reset")
	}
}

func TestQualityBufferIsBounded(t *testing.T) {
	var b qualityBuffer
	if _, err := b.Write(make([]byte, qualityOutputLimit)); err != nil {
		t.Fatal(err)
	}
	if _, err := b.Write([]byte("x")); err == nil || b.Len() != qualityOutputLimit {
		t.Fatal("unbounded probe output")
	}
	var copied qualityBuffer
	// os/exec uses io.Copy for the child pipe. Hide WriterTo on the input to
	// exercise the destination's optional ReaderFrom optimization too.
	source := io.LimitReader(strings.NewReader(strings.Repeat("x", qualityOutputLimit+1)), qualityOutputLimit+1)
	if _, err := io.Copy(&copied, source); err == nil || copied.Len() > qualityOutputLimit {
		t.Fatal("pipe copying bypassed the output limit")
	}
}

func newQualitySession(t *testing.T) (*Session, <-chan message) {
	t.Helper()
	received := make(chan message, 16)
	done := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		peer, err := (&websocket.Upgrader{}).Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer peer.Close()
		defer close(done)
		for {
			var m message
			if peer.ReadJSON(&m) != nil {
				return
			}
			received <- m
		}
	}))
	conn, _, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(server.URL, "http"), nil)
	if err != nil {
		server.Close()
		t.Fatal(err)
	}
	s := &Session{conn: conn, stopped: make(chan struct{}), events: make(chan Event, 32), quality: "2k", started: time.Now(), initial: &wyzeapi.LakeSession{UserID: "quality-test-user"}}
	t.Cleanup(func() {
		s.Close()
		server.Close()
		select {
		case <-done:
		case <-time.After(time.Second):
			t.Error("quality websocket leaked")
		}
	})
	return s, received
}

func TestQualityRecoveryUsesOnlyTheExistingSession(t *testing.T) {
	s, requests := newQualitySession(t)
	other, otherRequests := newQualitySession(t)
	b := NewBroker(context.Background(), nil)
	defer b.Close()
	b.sessions["porch"], b.sessions["other"] = s, other
	now := time.Unix(10000, 0)
	b.guardQuality("porch", s, lowQualitySample(), nil, now)
	select {
	case <-requests:
		t.Fatal("one low sample sent controls")
	default:
	}
	b.guardQuality("porch", s, lowQualitySample(), nil, now.Add(time.Minute))
	select {
	case m := <-requests:
		payload, err := base64.StdEncoding.DecodeString(str(m.Body["payload"]))
		if err != nil || m.Type != "data_stream" || !strings.Contains(string(payload), `"camera::resolution":"2k"`) || !strings.Contains(string(payload), "sight-safe::check-user") {
			t.Fatal("did not send high quality on existing gateway socket")
		}
	case <-time.After(time.Second):
		t.Fatal("confirmed downgrade did not request quality")
	}
	if b.Active() != 2 || b.sessions["porch"] != s {
		t.Fatal("recovery replaced a camera session")
	}
	select {
	case <-otherRequests:
		t.Fatal("quality recovery touched another camera")
	default:
	}
	select {
	case <-s.Done():
		t.Fatal("quality control closed its media session")
	default:
	}
	for i := 2; i < 6; i++ {
		b.guardQuality("porch", s, lowQualitySample(), nil, now.Add(time.Duration(i)*time.Minute))
	}
	select {
	case <-requests:
		t.Fatal("cooldown sent duplicate quality requests")
	default:
	}
}

func TestQualityResultsFromRetiredSessionAreDiscarded(t *testing.T) {
	old, oldRequests := newQualitySession(t)
	current, currentRequests := newQualitySession(t)
	b := NewBroker(context.Background(), nil)
	defer b.Close()
	b.sessions["front"] = current
	now := time.Unix(10000, 0)
	b.guardQuality("front", old, lowQualitySample(), nil, now)
	b.guardQuality("front", old, lowQualitySample(), nil, now.Add(time.Minute))
	if len(b.qualityPolicies) != 0 {
		t.Fatal("retired sample polluted current camera policy")
	}
	select {
	case <-oldRequests:
		t.Fatal("retired session received controls")
	default:
	}
	select {
	case <-currentRequests:
		t.Fatal("retired sample controlled replacement session")
	default:
	}
}

func TestQualityProbeErrorIsRedactedAndBreaksStreak(t *testing.T) {
	s, requests := newQualitySession(t)
	b := NewBroker(context.Background(), nil)
	defer b.Close()
	b.sessions["front"] = s
	now := time.Unix(10000, 0)
	b.guardQuality("front", s, lowQualitySample(), nil, now)
	b.guardQuality("front", s, ResolutionSample{}, errors.New("rtsp://private-token/private-user"), now.Add(time.Minute))
	b.guardQuality("front", s, lowQualitySample(), nil, now.Add(2*time.Minute))
	select {
	case <-requests:
		t.Fatal("probe error triggered camera control")
	default:
	}
	for len(s.events) > 0 {
		event := <-s.events
		if strings.Contains(fmt.Sprint(event), "private") {
			t.Fatal("diagnostics exposed probe error")
		}
	}
}

func TestQualityControlFailureStillConsumesBudget(t *testing.T) {
	s, _ := newQualitySession(t)
	b := NewBroker(context.Background(), nil)
	defer b.Close()
	b.sessions["front"] = s
	// A failed/uncertain write must not become an unbounded retry loop.
	s.conn.Close()
	now := time.Unix(10000, 0)
	b.guardQuality("front", s, lowQualitySample(), nil, now)
	b.guardQuality("front", s, lowQualitySample(), nil, now.Add(time.Minute))
	if len(b.qualityPolicies["front"].requests) != 1 {
		t.Fatal("failed write did not retain its rate-limit reservation")
	}
	found := false
	for len(s.events) > 0 {
		if (<-s.events).Kind == "quality_request_failed" {
			found = true
		}
	}
	if !found {
		t.Fatal("failed write was not reported")
	}
}

func TestQualityBudgetSurvivesSessionReplacement(t *testing.T) {
	s, _ := newQualitySession(t)
	b := NewBroker(context.Background(), nil)
	defer b.Close()
	b.sessions["front"] = s
	now := time.Unix(10000, 0)
	b.qualityPolicies = map[string]*qualityPolicy{"front": {consecutive: 1, lastSample: now, requests: []time.Time{now}}}
	b.ProbeQuality = func(context.Context, string) (ResolutionSample, error) {
		t.Error("canceled monitor probed")
		return ResolutionSample{}, nil
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	b.startQualityGuard(ctx, "front", s)
	p := b.qualityPolicies["front"]
	if p.consecutive != 0 || !p.lastSample.IsZero() || len(p.requests) != 1 || !p.requests[0].Equal(now) {
		t.Fatal("new session reset camera-level recovery budget")
	}
}

func TestCanceledProbeResultCannotControlCamera(t *testing.T) {
	s, requests := newQualitySession(t)
	b := NewBroker(context.Background(), nil)
	defer b.Close()
	b.sessions["front"] = s
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	b.checkQuality(ctx, "front", s, func(context.Context, string) (ResolutionSample, error) { cancel(); return lowQualitySample(), nil })
	if len(b.qualityPolicies) != 0 {
		t.Fatal("canceled probe updated the recovery policy")
	}
	select {
	case <-requests:
		t.Fatal("canceled probe controlled camera")
	default:
	}
}

func TestQualityGuardCancellationBeforeStartup(t *testing.T) {
	s, _ := newQualitySession(t)
	b := NewBroker(context.Background(), nil)
	defer b.Close()
	b.sessions["front"] = s
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	var calls atomic.Int32
	go func() {
		b.runQualityGuard(ctx, "front", s, func(context.Context, string) (ResolutionSample, error) { calls.Add(1); return lowQualitySample(), nil })
		close(done)
	}()
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("quality monitor survived cancellation")
	}
	if calls.Load() != 0 {
		t.Fatal("probe ran during startup grace")
	}
}

func TestQualityGuardStopsWithSession(t *testing.T) {
	s, _ := newQualitySession(t)
	b := NewBroker(context.Background(), nil)
	defer b.Close()
	b.sessions["front"] = s
	done := make(chan struct{})
	go func() {
		b.runQualityGuard(context.Background(), "front", s, func(context.Context, string) (ResolutionSample, error) {
			t.Error("unexpected startup probe")
			return ResolutionSample{}, nil
		})
		close(done)
	}()
	s.Close()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("quality monitor outlived camera session")
	}
}

func TestQualityGuardDoesNotProbeSD(t *testing.T) {
	s, _ := newQualitySession(t)
	s.quality = "360p"
	b := NewBroker(context.Background(), nil)
	defer b.Close()
	b.sessions["front"] = s
	b.ProbeQuality = func(context.Context, string) (ResolutionSample, error) {
		t.Error("SD camera was probed")
		return ResolutionSample{}, nil
	}
	b.startQualityGuard(context.Background(), "front", s)
	b.checkQuality(context.Background(), "front", s, b.ProbeQuality)
	if len(b.qualityPolicies) != 0 {
		t.Fatal("SD camera allocated a recovery policy")
	}
}

func fakeFFprobe(t *testing.T, body string) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("POSIX helper used for Linux runtime process checks")
	}
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "ffprobe"), []byte("#!/bin/sh\n"+body+"\n"), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
}

func TestNativeProbeParsesDecodedFrames(t *testing.T) {
	file := filepath.Join(t.TempDir(), "frames.json")
	raw := resolutionFixture(func(i int) (int, int) {
		if i%20 == 0 {
			return 3840, 2160
		}
		return 640, 360
	})
	if err := os.WriteFile(file, raw, 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("QUALITY_FIXTURE", file)
	fakeFFprobe(t, `cat "$QUALITY_FIXTURE"`)
	sample, err := NewResolutionProbe(8554)(context.Background(), "front")
	if err != nil || !sample.low() {
		t.Fatalf("native probe did not catch mixed-resolution regression: %+v %v", sample, err)
	}
}

func TestNativeProbeKeepsSourceLoopbackAndPassesNoShell(t *testing.T) {
	fixture := filepath.Join(t.TempDir(), "frames.json")
	argsFile := filepath.Join(t.TempDir(), "source.txt")
	if err := os.WriteFile(fixture, resolutionFixture(func(int) (int, int) { return 3840, 2160 }), 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("QUALITY_FIXTURE", fixture)
	t.Setenv("QUALITY_ARGS", argsFile)
	fakeFFprobe(t, `for last; do :; done
printf '%s' "$last" > "$QUALITY_ARGS"
cat "$QUALITY_FIXTURE"`)
	if _, err := NewResolutionProbe(8554)(context.Background(), "porch?src=rtsp://untrusted/"); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(argsFile)
	if err != nil {
		t.Fatal(err)
	}
	source, err := url.Parse(string(raw))
	if err != nil || source.Scheme != "rtsp" || source.Host != "127.0.0.1:8554" || source.RawQuery != "" {
		t.Fatal("camera name changed probe destination")
	}
}

func TestNativeProbeCancellationAndRedaction(t *testing.T) {
	t.Run("cancellation", func(t *testing.T) {
		fakeFFprobe(t, "exec sleep 30")
		ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
		defer cancel()
		start := time.Now()
		if _, err := NewResolutionProbe(8554)(ctx, "front"); err == nil {
			t.Fatal("canceled probe succeeded")
		}
		if time.Since(start) > 2*time.Second {
			t.Fatal("probe child survived cancellation")
		}
	})
	t.Run("redaction", func(t *testing.T) {
		fakeFFprobe(t, "echo private-test-token >&2\nexit 1")
		_, err := NewResolutionProbe(8554)(context.Background(), "private-test-user")
		if err == nil || strings.Contains(err.Error(), "private") {
			t.Fatal("probe leaked diagnostics/URL")
		}
	})
	for _, port := range []int{0, -1, 65536} {
		if _, err := NewResolutionProbe(port)(context.Background(), "front"); err == nil {
			t.Fatal("invalid RTSP port accepted")
		}
	}
}

// Explicit opt-in: probes existing loopback RTSP only; never sends controls.
func TestNativeResolutionLiveQA(t *testing.T) {
	if os.Getenv("WYZE_NATIVE_RESOLUTION_QA") != "1" {
		t.Skip("opt-in read-only live RTSP QA")
	}
	sample, err := NewResolutionProbe(8554)(context.Background(), "front")
	if err != nil || sample.low() || sample.Width != 3840 || sample.Height != 2160 {
		t.Fatalf("native live frame check failed: %+v %v", sample, err)
	}
	t.Logf("decoded %d steady frames: %dx%d; low frames %d", sample.Frames, sample.Width, sample.Height, sample.LowFrames)
}

func TestOwnershipHeartbeatDoesNotRewriteQuality(t *testing.T) {
	s, _, requests := newAsyncRenewalSession(t, nil)
	timer := time.NewTimer(27 * time.Second)
	defer timer.Stop()
	controls := 0
	for controls < 2 {
		select {
		case request := <-requests:
			if request.Type != "data_stream" {
				continue
			}
			payload, err := base64.StdEncoding.DecodeString(str(request.Body["payload"]))
			if err != nil || !strings.Contains(string(payload), "sight-safe::check-user") {
				t.Fatal("ownership heartbeat was lost")
			}
			if strings.Contains(string(payload), "camera::resolution") {
				t.Fatal("periodic heartbeat bypassed the quality guard")
			}
			controls++
		case <-s.Done():
			t.Fatal("ownership check closed its existing session")
		case <-timer.C:
			t.Fatal("missing periodic ownership heartbeats")
		}
	}
}
