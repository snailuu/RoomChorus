package syncctrl

import (
	"math"
	"sort"
	"sync"
	"time"
)

const (
	MinTargetMs         = 250.0
	MaxTargetMs         = 2000.0
	DefaultTargetMs     = 300.0
	HeadroomMs          = 60.0
	QuantStepMs         = 50.0
	RaiseHoldDuration   = 4 * time.Second
	LowerHoldDuration   = 30 * time.Second
	MaxSlewMsPerSec     = 8.0
	TelemetryStaleAfter = 15 * time.Second
	MedianWindow        = 9
	EmaAlpha            = 0.2
	MinSamplesForTarget = 2
	MaxSendDelayMs      = 1800.0
)

// ChannelMode defines per-listener stereo channel routing applied on the host.
type ChannelMode string

const (
	ChannelStereo ChannelMode = "stereo"
	ChannelLeft   ChannelMode = "left"
	ChannelRight  ChannelMode = "right"
	ChannelMono   ChannelMode = "mono"
)

// ValidChannelMode normalizes an incoming channel mode string.
func ValidChannelMode(mode string) ChannelMode {
	switch ChannelMode(mode) {
	case ChannelLeft:
		return ChannelLeft
	case ChannelRight:
		return ChannelRight
	case ChannelMono:
		return ChannelMono
	default:
		return ChannelStereo
	}
}

// ListenerTelemetry holds raw measurements and preferences reported by a listener browser.
type ListenerTelemetry struct {
	WsRttMs            float64     `json:"wsRttMs"`
	JitterBufferMs     float64     `json:"jitterBufferMs"`
	AudioOutputMs      float64     `json:"audioOutputMs"`
	PacketsLost        uint64      `json:"packetsLost"`
	PacketsReceived    uint64      `json:"packetsReceived"`
	ClockOffsetMs      float64     `json:"clockOffsetMs"`
	UserOffsetMs       float64     `json:"userOffsetMs"`
	ChannelMode        ChannelMode `json:"channelMode"`
	ClientGain         float64     `json:"clientGain"`
	ClientCanSetVolume bool        `json:"clientCanSetVolume"`
	Playing            bool        `json:"playing"`
	Background         bool        `json:"background"`
}

// ListenerSyncState describes a listener's filtered latency path and scheduled compensation.
type ListenerSyncState struct {
	ID                  string            `json:"id"`
	Name                string            `json:"name"`
	UserAgent           string            `json:"userAgent"`
	RawBasePathMs       float64           `json:"rawBasePathMs"`
	FilteredBasePathMs  float64           `json:"filteredBasePathMs"`
	EffectiveBasePathMs float64           `json:"effectiveBasePathMs"`
	SendDelayMs         float64           `json:"sendDelayMs"`
	EstimatedSkewMs     float64           `json:"estimatedSkewMs"`
	LastTelemetryAt     time.Time         `json:"lastTelemetryAt"`
	SamplesCount        int               `json:"samplesCount"`
	Telemetry           ListenerTelemetry `json:"telemetry"`
}

// Snapshot is the immutable synchronization state published to the host UI and listeners.
type Snapshot struct {
	DisplayTargetMs     float64             `json:"displayTargetMs"`
	ActiveTargetMs      float64             `json:"activeTargetMs"`
	HostMonitorEnabled  bool                `json:"hostMonitorEnabled"`
	HostOutputLatencyMs float64             `json:"hostOutputLatencyMs"`
	HostUserOffsetMs    float64             `json:"hostUserOffsetMs"`
	HostDelayMs         float64             `json:"hostDelayMs"`
	OpusLookaheadMs     float64             `json:"opusLookaheadMs"`
	Listeners           []ListenerSyncState `json:"listeners"`
}

type listenerEntry struct {
	state   ListenerSyncState
	history []float64
}

// Controller computes the shared playback timeline target and per-device send delays.
type Controller struct {
	mu sync.Mutex

	opusLookaheadMs     float64
	hostMonitorEnabled  bool
	hostOutputLatencyMs float64
	hostUserOffsetMs    float64

	displayTargetMs float64
	activeTargetMs  float64
	lastTickAt      time.Time

	pendingDirection int // +1 raise, -1 lower, 0 none
	pendingSince     time.Time

	listeners map[string]*listenerEntry
}

// NewController initializes a synchronization controller with default timeline bounds.
func NewController(opusLookaheadMs float64) *Controller {
	return &Controller{
		opusLookaheadMs: opusLookaheadMs,
		displayTargetMs: DefaultTargetMs,
		activeTargetMs:  DefaultTargetMs,
		listeners:       make(map[string]*listenerEntry),
	}
}

// SetOpusLookaheadMs updates the algorithmic Opus encoder lookahead in milliseconds.
func (c *Controller) SetOpusLookaheadMs(ms float64) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if ms >= 0 && ms < 100 {
		c.opusLookaheadMs = ms
	}
}

// SetHostMonitor updates local host monitor enablement, hardware output latency, and manual offset.
func (c *Controller) SetHostMonitor(enabled bool, outputLatencyMs, userOffsetMs float64) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.hostMonitorEnabled = enabled
	if outputLatencyMs < 0 {
		outputLatencyMs = 0
	}
	c.hostOutputLatencyMs = outputLatencyMs
	c.hostUserOffsetMs = clamp(userOffsetMs, -500, 500)
}

// RegisterListener registers a newly connected listener with sensible defaults.
func (c *Controller) RegisterListener(id, name, userAgent string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.listeners[id] = &listenerEntry{
		state: ListenerSyncState{
			ID:        id,
			Name:      name,
			UserAgent: userAgent,
			Telemetry: ListenerTelemetry{
				ChannelMode:        ChannelStereo,
				ClientGain:         1.0,
				ClientCanSetVolume: true,
			},
		},
	}
}

// RemoveListener removes a disconnected listener.
func (c *Controller) RemoveListener(id string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.listeners, id)
}

// UpdatePrefs updates per-listener playback preferences (channel, manual offset, gain, name).
func (c *Controller) UpdatePrefs(id string, channel ChannelMode, userOffsetMs float64, clientGain float64, canSetVol *bool, name string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	entry, ok := c.listeners[id]
	if !ok {
		return
	}
	if name != "" {
		entry.state.Name = name
	}
	entry.state.Telemetry.ChannelMode = ValidChannelMode(string(channel))
	entry.state.Telemetry.UserOffsetMs = clamp(userOffsetMs, -500, 500)
	entry.state.Telemetry.ClientGain = clamp(clientGain, 0, 1)
	if canSetVol != nil {
		entry.state.Telemetry.ClientCanSetVolume = *canSetVol
	}
	entry.state.EffectiveBasePathMs = entry.state.FilteredBasePathMs - entry.state.Telemetry.UserOffsetMs
	c.recomputeDelaysLocked()
}

// ReportTelemetry ingests a listener WebRTC/WS measurement sample and updates filtered path latency.
func (c *Controller) ReportTelemetry(id string, tel ListenerTelemetry, now time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	entry, ok := c.listeners[id]
	if !ok {
		return
	}

	// Preserve server-managed prefs if telemetry doesn't override them.
	if tel.ChannelMode == "" {
		tel.ChannelMode = entry.state.Telemetry.ChannelMode
	} else {
		tel.ChannelMode = ValidChannelMode(string(tel.ChannelMode))
	}
	if tel.ClientGain <= 0 && !tel.Playing && entry.state.SamplesCount == 0 {
		tel.ClientGain = entry.state.Telemetry.ClientGain
	}
	tel.ClientGain = clamp(tel.ClientGain, 0, 1)
	tel.UserOffsetMs = clamp(tel.UserOffsetMs, -500, 500)
	if tel.WsRttMs < 0 {
		tel.WsRttMs = 0
	}
	if tel.JitterBufferMs < 0 {
		tel.JitterBufferMs = 0
	}
	if tel.AudioOutputMs < 0 {
		tel.AudioOutputMs = 0
	}

	wasPlaying := entry.state.Telemetry.Playing
	if !wasPlaying && tel.Playing {
		entry.history = nil
		entry.state.SamplesCount = 0
		entry.state.FilteredBasePathMs = 0
	}

	entry.state.Telemetry = tel
	entry.state.LastTelemetryAt = now

	if !tel.Playing || tel.Background {
		entry.state.EffectiveBasePathMs = entry.state.FilteredBasePathMs - tel.UserOffsetMs
		c.recomputeDelaysLocked()
		return
	}

	jb := tel.JitterBufferMs
	if entry.state.SamplesCount > 0 && jb > 400.0 && jb > entry.state.FilteredBasePathMs*3.0 {
		jb = 400.0
	}

	rawBase := c.opusLookaheadMs + (tel.WsRttMs / 2.0) + jb + tel.AudioOutputMs
	entry.state.RawBasePathMs = rawBase

	entry.history = append(entry.history, rawBase)
	if len(entry.history) > MedianWindow {
		entry.history = entry.history[len(entry.history)-MedianWindow:]
	}
	med := median(entry.history)
	if entry.state.SamplesCount == 0 {
		entry.state.FilteredBasePathMs = med
	} else {
		entry.state.FilteredBasePathMs = EmaAlpha*med + (1.0-EmaAlpha)*entry.state.FilteredBasePathMs
	}
	entry.state.SamplesCount++
	entry.state.EffectiveBasePathMs = entry.state.FilteredBasePathMs - tel.UserOffsetMs

	c.recomputeDelaysLocked()
}

// Tick evaluates the slowest active path, applies raise/lower hold hysteresis, and slews ActiveTargetMs.
func (c *Controller) Tick(now time.Time) Snapshot {
	c.mu.Lock()
	defer c.mu.Unlock()

	slowestBase, hasActive := c.slowestEffectiveBaseLocked(now)
	if hasActive {
		desired := quantizeTarget(slowestBase + HeadroomMs)
		if desired > c.displayTargetMs {
			if c.pendingDirection != 1 {
				c.pendingDirection = 1
				c.pendingSince = now
			} else if now.Sub(c.pendingSince) >= RaiseHoldDuration {
				c.displayTargetMs = desired
				c.pendingDirection = 0
			}
		} else if desired <= c.displayTargetMs-QuantStepMs {
			if c.pendingDirection != -1 {
				c.pendingDirection = -1
				c.pendingSince = now
			} else if now.Sub(c.pendingSince) >= LowerHoldDuration {
				c.displayTargetMs = desired
				c.pendingDirection = 0
			}
		} else {
			c.pendingDirection = 0
		}
	} else {
		c.pendingDirection = 0
	}

	if c.lastTickAt.IsZero() {
		c.lastTickAt = now
	}
	dt := now.Sub(c.lastTickAt).Seconds()
	if dt < 0 {
		dt = 0
	}
	if dt > 2.0 {
		dt = 2.0
	}
	c.lastTickAt = now

	maxStep := MaxSlewMsPerSec * dt
	diff := c.displayTargetMs - c.activeTargetMs
	if math.Abs(diff) <= maxStep {
		c.activeTargetMs = c.displayTargetMs
	} else if diff > 0 {
		c.activeTargetMs += maxStep
	} else {
		c.activeTargetMs -= maxStep
	}

	c.recomputeDelaysLocked()
	return c.snapshotLocked()
}

// Snapshot returns the current synchronization snapshot without advancing time.
func (c *Controller) Snapshot() Snapshot {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.snapshotLocked()
}

// ListenerSendDelay returns the current scheduled send delay (ms) and prefs for a listener.
func (c *Controller) ListenerSendDelay(id string) (sendDelayMs float64, mode ChannelMode, clientGain float64, canSetVol bool, ok bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	entry, exists := c.listeners[id]
	if !exists {
		return 0, ChannelStereo, 1.0, true, false
	}
	return entry.state.SendDelayMs,
		entry.state.Telemetry.ChannelMode,
		entry.state.Telemetry.ClientGain,
		entry.state.Telemetry.ClientCanSetVolume,
		true
}

// HostSendDelay returns the active target delay for HostMonitor playback in milliseconds.
func (c *Controller) HostSendDelay() float64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.hostDelayLocked()
}

func (c *Controller) slowestEffectiveBaseLocked(now time.Time) (float64, bool) {
	var slowest float64
	var any bool

	if c.hostMonitorEnabled {
		hostEff := c.hostOutputLatencyMs - c.hostUserOffsetMs
		if hostEff < 0 {
			hostEff = 0
		}
		slowest = hostEff
		any = true
	}

	for _, entry := range c.listeners {
		s := entry.state
		if !s.Telemetry.Playing || s.Telemetry.Background {
			continue
		}
		if s.SamplesCount < MinSamplesForTarget {
			continue
		}
		if now.Sub(s.LastTelemetryAt) > TelemetryStaleAfter {
			continue
		}
		eff := s.EffectiveBasePathMs
		if eff < 0 {
			eff = 0
		}
		if !any || eff > slowest {
			slowest = eff
			any = true
		}
	}

	return slowest, any
}

func (c *Controller) hostDelayLocked() float64 {
	if !c.hostMonitorEnabled {
		return 0
	}
	hostEff := c.hostOutputLatencyMs - c.hostUserOffsetMs
	return clamp(c.activeTargetMs-hostEff, 0, MaxSendDelayMs)
}

func (c *Controller) recomputeDelaysLocked() {
	for _, entry := range c.listeners {
		s := &entry.state
		s.EffectiveBasePathMs = s.FilteredBasePathMs - s.Telemetry.UserOffsetMs
		s.SendDelayMs = clamp(c.activeTargetMs-s.EffectiveBasePathMs, 0, MaxSendDelayMs)
		s.EstimatedSkewMs = (s.SendDelayMs + s.EffectiveBasePathMs) - c.activeTargetMs
	}
}

func (c *Controller) snapshotLocked() Snapshot {
	list := make([]ListenerSyncState, 0, len(c.listeners))
	for _, entry := range c.listeners {
		list = append(list, entry.state)
	}
	sort.Slice(list, func(i, j int) bool {
		return list[i].ID < list[j].ID
	})

	return Snapshot{
		DisplayTargetMs:     c.displayTargetMs,
		ActiveTargetMs:      c.activeTargetMs,
		HostMonitorEnabled:  c.hostMonitorEnabled,
		HostOutputLatencyMs: c.hostOutputLatencyMs,
		HostUserOffsetMs:    c.hostUserOffsetMs,
		HostDelayMs:         c.hostDelayLocked(),
		OpusLookaheadMs:     c.opusLookaheadMs,
		Listeners:           list,
	}
}

func quantizeTarget(ms float64) float64 {
	q := math.Ceil(ms/QuantStepMs) * QuantStepMs
	return clamp(q, MinTargetMs, MaxTargetMs)
}

func median(vals []float64) float64 {
	if len(vals) == 0 {
		return 0
	}
	cp := make([]float64, len(vals))
	copy(cp, vals)
	sort.Float64s(cp)
	mid := len(cp) / 2
	if len(cp)%2 == 1 {
		return cp[mid]
	}
	return (cp[mid-1] + cp[mid]) / 2.0
}

func clamp(v, lo, hi float64) float64 {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}
