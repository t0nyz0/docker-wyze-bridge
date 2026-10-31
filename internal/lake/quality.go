package lake

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"math"
	"net"
	"net/url"
	"os/exec"
	"strconv"
	"time"
)

const (
	qualityStartupGrace  = 2 * time.Minute
	qualityCheckInterval = time.Minute
	qualityProbeTimeout  = 25 * time.Second
	qualityCooldown      = 15 * time.Minute
	qualityHourlyLimit   = 3
	qualityOutputLimit   = 512 * 1024
)

// ResolutionSample describes decoded frames after decoder startup, not the
// cached stream/SPS dimensions. Occasional high-resolution frames do not make
// a predominantly low-resolution sample healthy.
type ResolutionSample struct {
	Width, Height             int
	Frames, LowFrames         int
	TailFrames, TailLowFrames int
}

func (s ResolutionSample) low() bool {
	return s.Frames >= 30 && s.TailFrames > 0 &&
		2*s.LowFrames >= s.Frames && 2*s.TailLowFrames >= s.TailFrames
}

type ResolutionProbe func(context.Context, string) (ResolutionSample, error)

type decodedFrame struct {
	Width  int    `json:"width"`
	Height int    `json:"height"`
	PTS    string `json:"best_effort_timestamp_time"`
}

// NewResolutionProbe reads the existing embedded go2rtc output. It does not
// provision another manufacturer session and never reads account credentials.
func NewResolutionProbe(rtspPort int) ResolutionProbe {
	return func(ctx context.Context, camera string) (ResolutionSample, error) {
		if rtspPort < 1 || rtspPort > 65535 {
			return ResolutionSample{}, errors.New("invalid local RTSP port")
		}
		source := (&url.URL{Scheme: "rtsp", Host: net.JoinHostPort("127.0.0.1", strconv.Itoa(rtspPort)), Path: "/" + camera + "_4k"}).String()
		ctx, cancel := context.WithTimeout(ctx, qualityProbeTimeout)
		defer cancel()
		command := exec.CommandContext(ctx, "ffprobe", "-v", "error", "-rtsp_transport", "tcp",
			"-rw_timeout", "8000000", "-threads", "2", "-select_streams", "v:0",
			"-read_intervals", "%+12", "-show_frames", "-show_entries",
			"frame=width,height,best_effort_timestamp_time", "-of", "json", source)
		output := &qualityBuffer{}
		command.Stdout, command.Stderr = output, io.Discard
		command.WaitDelay = 2 * time.Second
		if command.Run() != nil {
			// Neither FFmpeg diagnostics nor command arguments enter protocol logs.
			return ResolutionSample{}, errors.New("native resolution probe failed")
		}
		return parseResolutionSample(output.Bytes())
	}
}

// Do not embed bytes.Buffer: its promoted ReadFrom would let io.Copy bypass
// Write's limit when os/exec drains the subprocess pipe.
type qualityBuffer struct{ buffer bytes.Buffer }

func (b *qualityBuffer) Bytes() []byte { return b.buffer.Bytes() }
func (b *qualityBuffer) Len() int      { return b.buffer.Len() }

func (b *qualityBuffer) Write(p []byte) (int, error) {
	if len(p) > qualityOutputLimit-b.Len() {
		return 0, errors.New("resolution sample exceeds output limit")
	}
	return b.buffer.Write(p)
}

func parseResolutionSample(raw []byte) (ResolutionSample, error) {
	var data struct {
		Frames []decodedFrame `json:"frames"`
	}
	if len(raw) > qualityOutputLimit || json.Unmarshal(raw, &data) != nil {
		return ResolutionSample{}, errors.New("invalid decoded resolution sample")
	}
	type frame struct {
		seconds       float64
		width, height int
	}
	var steady []frame
	var origin, previous float64
	haveOrigin := false
	for _, f := range data.Frames {
		pts, err := strconv.ParseFloat(f.PTS, 64)
		if err != nil || math.IsNaN(pts) || math.IsInf(pts, 0) || f.Width <= 0 || f.Height <= 0 {
			continue
		}
		if !haveOrigin {
			origin, previous, haveOrigin = pts, pts, true
		}
		if pts < previous {
			return ResolutionSample{}, errors.New("nonmonotonic decoded resolution sample")
		}
		previous = pts
		// Fresh decoders can initially expose stale cached parameter sets.
		if pts-origin >= 3 {
			steady = append(steady, frame{pts - origin, f.Width, f.Height})
		}
	}
	if len(steady) < 30 || steady[len(steady)-1].seconds-steady[0].seconds < 6 {
		return ResolutionSample{}, errors.New("insufficient steady decoded video")
	}
	last := steady[len(steady)-1]
	sample := ResolutionSample{Width: last.width, Height: last.height, Frames: len(steady)}
	for _, f := range steady {
		low := f.width < 1280 || f.height < 720
		if low {
			sample.LowFrames++
		}
		if f.seconds >= last.seconds-2 {
			sample.TailFrames++
			if low {
				sample.TailLowFrames++
			}
		}
	}
	return sample, nil
}

type qualityPolicy struct {
	consecutive int
	lastSample  time.Time
	requests    []time.Time
}

func (p *qualityPolicy) observe(now time.Time, sample ResolutionSample, err error) bool {
	// An unknown sample breaks the streak and never causes camera control.
	if err != nil || !sample.low() {
		p.consecutive = 0
		p.lastSample = now
		return false
	}
	if !p.lastSample.IsZero() && (now.Sub(p.lastSample) < 45*time.Second || now.Sub(p.lastSample) > 3*time.Minute) {
		p.consecutive = 0
	}
	p.lastSample = now
	p.consecutive++
	if p.consecutive < 2 {
		return false
	}
	var recent []time.Time
	for _, request := range p.requests {
		if now.Sub(request) < time.Hour {
			recent = append(recent, request)
		}
	}
	p.requests = recent
	if len(recent) >= qualityHourlyLimit || (len(recent) > 0 && now.Sub(recent[len(recent)-1]) < qualityCooldown) {
		return false
	}
	// Reserve the attempt before sending, including failures/uncertain results.
	p.requests = append(p.requests, now)
	p.consecutive = 0
	return true
}

func (b *Broker) ownsQualitySession(name string, session *Session) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.sessions[name] == session && b.ctx.Err() == nil
}

func (b *Broker) startQualityGuard(ctx context.Context, name string, session *Session) {
	probe := b.ProbeQuality
	if probe == nil || session.quality != "2k" {
		return
	}
	b.mu.Lock()
	if b.qualityPolicies == nil {
		b.qualityPolicies = map[string]*qualityPolicy{}
	}
	policy := b.qualityPolicies[name]
	if policy == nil {
		policy = &qualityPolicy{}
		b.qualityPolicies[name] = policy
	}
	// New sessions get grace and a new streak, but keep camera-level rate history.
	policy.consecutive, policy.lastSample = 0, time.Time{}
	b.mu.Unlock()
	go b.runQualityGuard(ctx, name, session, probe)
}

func (b *Broker) runQualityGuard(parent context.Context, name string, session *Session, probe ResolutionProbe) {
	ctx, cancel := context.WithCancel(parent)
	defer cancel()
	go func() {
		select {
		case <-session.Done():
			cancel()
		case <-ctx.Done():
		}
	}()
	timer := time.NewTimer(qualityStartupGrace)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
			b.checkQuality(ctx, name, session, probe)
			timer.Reset(qualityCheckInterval)
		}
	}
}

func (b *Broker) checkQuality(ctx context.Context, name string, session *Session, probe ResolutionProbe) {
	if ctx.Err() != nil || session.quality != "2k" || !b.ownsQualitySession(name, session) {
		return
	}
	sample, err := probe(ctx, name)
	if ctx.Err() != nil {
		return
	}
	b.guardQuality(name, session, sample, err, time.Now())
}

func (b *Broker) guardQuality(name string, session *Session, sample ResolutionSample, err error, now time.Time) {
	b.mu.Lock()
	if b.sessions[name] != session || b.ctx.Err() != nil {
		b.mu.Unlock()
		return
	}
	if b.qualityPolicies == nil {
		b.qualityPolicies = map[string]*qualityPolicy{}
	}
	policy := b.qualityPolicies[name]
	if policy == nil {
		policy = &qualityPolicy{}
		b.qualityPolicies[name] = policy
	}
	request := policy.observe(now, sample, err)
	b.mu.Unlock()
	if err != nil {
		session.emit(Event{Kind: "quality_probe_failed"})
		return
	}
	session.emit(Event{Kind: "quality_sample", Width: sample.Width, Height: sample.Height, Frames: sample.Frames, LowFrames: sample.LowFrames})
	if !request || !b.ownsQualitySession(name, session) {
		return
	}
	select {
	case <-session.Done():
		return
	default:
	}
	if session.controls(true) != nil {
		session.emit(Event{Kind: "quality_request_failed"})
		return
	}
	session.emit(Event{Kind: "quality_requested"})
}
