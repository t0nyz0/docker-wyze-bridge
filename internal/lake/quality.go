package lake

import (
	"context"
	"time"
)

type Resolution struct{ Width, Height int }

const (
	qualityWarmup       = 2 * time.Minute
	qualityInterval     = time.Minute
	qualityCooldown     = 2 * time.Minute
	qualityBudgetWindow = time.Hour
	qualityMaxAttempts  = 3
)

type qualityPolicy struct {
	target         Resolution // Zero means the Pan v4's 3840x2160.
	lowSamples     int
	attempts       []time.Time
	lastAttempt    time.Time
	low, exhausted bool
}

// guardsQuality reports whether a session asks for the camera's native
// resolution and so gets the quality guard; a "360p" session wants the drop.
func guardsQuality(quality string) bool {
	return quality == "2k" || quality == "2.5k"
}

// NativeResolution is the decoded size a camera sends once its quality label
// has taken effect: "2k" is the Pan v4's 3840x2160, "2.5k" the Bulb Cam Pan's
// 2560x1440. Anything smaller counts as a drop.
func NativeResolution(quality string) Resolution {
	if quality == "2.5k" {
		return Resolution{Width: 2560, Height: 1440}
	}
	return Resolution{Width: 3840, Height: 2160}
}

func (p *qualityPolicy) atTarget(resolution Resolution) bool {
	target := p.target
	if target.Width == 0 || target.Height == 0 {
		target = NativeResolution("")
	}
	return resolution.Width >= target.Width && resolution.Height >= target.Height
}

// observe requires two consecutive low samples. Errors/idle/transitions break
// that sequence. Recovery clears the warning, but does not refill the budget.
func (p *qualityPolicy) observe(now time.Time, resolution Resolution, err error) (event string, retry bool) {
	if err != nil || resolution.Width <= 0 || resolution.Height <= 0 {
		p.lowSamples = 0
		return "", false
	}
	if p.atTarget(resolution) {
		p.lowSamples = 0
		p.exhausted = false
		if p.low {
			p.low = false
			return "quality_recovered", false
		}
		return "", false
	}
	p.lowSamples++
	if p.lowSamples < 2 {
		return "", false
	}
	if !p.low {
		p.low = true
		event = "quality_low"
	}
	for len(p.attempts) > 0 && now.Sub(p.attempts[0]) >= qualityBudgetWindow {
		p.attempts = p.attempts[1:]
		p.exhausted = false
	}
	if !p.lastAttempt.IsZero() && now.Sub(p.lastAttempt) < qualityCooldown {
		return event, false
	}
	if len(p.attempts) >= qualityMaxAttempts {
		if !p.exhausted {
			p.exhausted = true
			return "quality_retry_exhausted", false
		}
		return event, false
	}
	p.attempts = append(p.attempts, now)
	p.lastAttempt = now
	return event, true
}

func (b *Broker) ownsQualitySession(name string, session *Session, operation *cameraOperation) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.ctx.Err() == nil && b.sessions[name] == session && b.pending[name] == operation && b.epochs[name] == operation.epoch
}

// requestQuality can only write to the captured, still-owned session. It never
// provisions a receiver or replaces a session. A concurrent close makes the
// write fail on the old socket; it cannot affect the replacement session.
func (b *Broker) requestQuality(name string, session *Session, operation *cameraOperation) bool {
	if !b.ownsQualitySession(name, session, operation) {
		return false
	}
	select {
	case <-session.Done():
		return false
	default:
	}
	if err := session.controls(true); err != nil {
		session.notify("quality_retry_send_failed", 0)
		return false
	}
	session.notify("quality_retry_sent", 0)
	return true
}

func (b *Broker) guardQuality(ctx context.Context, name string, session *Session, operation *cameraOperation) {
	b.runQualityGuard(ctx, name, session, operation, qualityWarmup, qualityInterval)
}

func (b *Broker) runQualityGuard(ctx context.Context, name string, session *Session, operation *cameraOperation, warmup, interval time.Duration) {
	// The guard belongs to this session, including while a probe is running.
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	go func() {
		select {
		case <-session.Done():
			cancel()
		case <-ctx.Done():
		}
	}()
	timer := time.NewTimer(warmup)
	defer timer.Stop()
	policy := qualityPolicy{target: NativeResolution(session.quality)}
	failedProbes := 0
	for {
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
		}
		if !b.ownsQualitySession(name, session, operation) {
			return
		}
		probeCtx, stop := context.WithTimeout(ctx, 25*time.Second)
		resolution, err := b.ProbeQuality(probeCtx, name)
		stop()
		if ctx.Err() != nil || !b.ownsQualitySession(name, session, operation) {
			return
		}
		if err != nil {
			failedProbes++
			if failedProbes == 3 {
				session.notify("quality_probe_unavailable", 0)
			}
		}
		if err == nil && resolution.Width > 0 && resolution.Height > 0 {
			failedProbes = 0
			select {
			case session.events <- Event{Kind: "quality_sample", Width: resolution.Width, Height: resolution.Height}:
			default:
			}
		}
		event, retry := policy.observe(time.Now(), resolution, err)
		if event != "" {
			session.notify(event, 0)
		}
		if retry {
			b.requestQuality(name, session, operation)
		}
		timer.Reset(interval)
	}
}
