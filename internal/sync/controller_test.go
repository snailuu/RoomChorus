package syncctrl

import (
	"math"
	"testing"
	"time"
)

func TestSyncControllerRaiseHysteresisAndSlew(t *testing.T) {
	c := NewController(6.5)
	base := time.Unix(1_700_000_000, 0)
	c.Tick(base)

	c.RegisterListener("iphone-1", "iPhone", "Safari")
	tel := ListenerTelemetry{
		WsRttMs:            10,
		JitterBufferMs:     80,
		AudioOutputMs:      200, // raw = 6.5 + 5 + 80 + 200 = 291.5ms -> desired = ceil((291.5+60)/50)*50 = 400ms
		ChannelMode:        ChannelStereo,
		ClientGain:         1.0,
		ClientCanSetVolume: false,
		Playing:            true,
	}

	// Report 3 samples within 2 seconds: DisplayTarget should NOT raise yet (< 4s hold).
	for i := 1; i <= 3; i++ {
		now := base.Add(time.Duration(i) * time.Second)
		c.ReportTelemetry("iphone-1", tel, now)
		snap := c.Tick(now)
		if snap.DisplayTargetMs != DefaultTargetMs {
			t.Fatalf("at %ds expected DisplayTargetMs=%v before 4s hold, got %v", i, DefaultTargetMs, snap.DisplayTargetMs)
		}
	}

	// After 4s continuous observation, DisplayTargetMs raises to 400ms.
	t5 := base.Add(6 * time.Second)
	c.ReportTelemetry("iphone-1", tel, t5)
	snap := c.Tick(t5)
	if snap.DisplayTargetMs != 400.0 {
		t.Fatalf("expected DisplayTargetMs=400.0 after hold, got %v", snap.DisplayTargetMs)
	}

	// ActiveTargetMs slews smoothly at max 25ms/s rather than jumping instantly to 400ms.
	if snap.ActiveTargetMs >= 400.0 {
		t.Fatalf("expected ActiveTargetMs to slew gradually, got %v", snap.ActiveTargetMs)
	}

	// Advance 15 seconds in 1s ticks -> should converge to 400ms (100ms delta at 8ms/s takes 12.5s).
	for i := 7; i <= 22; i++ {
		now := base.Add(time.Duration(i) * time.Second)
		c.ReportTelemetry("iphone-1", tel, now)
		snap = c.Tick(now)
	}
	if math.Abs(snap.ActiveTargetMs-400.0) > 0.01 {
		t.Fatalf("expected ActiveTargetMs to converge to 400.0, got %v", snap.ActiveTargetMs)
	}
}

func TestSyncControllerIgnoresPausedTelemetrySpikes(t *testing.T) {
	c := NewController(6.5)
	base := time.Unix(1_700_000_000, 0)
	c.Tick(base)

	c.RegisterListener("iphone", "iPhone", "Safari")

	// Pre-play paused frames can exhibit huge Safari jitterBufferDelay spikes (480ms); must be ignored.
	for i := 1; i <= 6; i++ {
		now := base.Add(time.Duration(i) * time.Second)
		c.ReportTelemetry("iphone", ListenerTelemetry{
			WsRttMs: 10, JitterBufferMs: 480, AudioOutputMs: 28, Playing: false,
		}, now)
		snap := c.Tick(now)
		if snap.DisplayTargetMs != DefaultTargetMs {
			t.Fatalf("paused telemetry must not raise target, got %v", snap.DisplayTargetMs)
		}
		if snap.Listeners[0].SamplesCount != 0 || snap.Listeners[0].FilteredBasePathMs != 0 {
			t.Fatalf("paused telemetry must not populate filtered path, got %+v", snap.Listeners[0])
		}
	}

	// Transition to Playing: true with real 50ms jitter buffer -> resets cleanly to ~89.5ms path.
	for i := 7; i <= 12; i++ {
		now := base.Add(time.Duration(i) * time.Second)
		c.ReportTelemetry("iphone", ListenerTelemetry{
			WsRttMs: 10, JitterBufferMs: 50, AudioOutputMs: 28, Playing: true,
		}, now)
		c.Tick(now)
	}
	snap := c.Snapshot()
	if snap.Listeners[0].FilteredBasePathMs > 95.0 || snap.Listeners[0].FilteredBasePathMs < 85.0 {
		t.Fatalf("expected clean post-play path ~89.5ms without pre-play 480ms poison, got %v", snap.Listeners[0].FilteredBasePathMs)
	}
}

func TestSyncControllerUserOffsetAndMultiDeviceAlignment(t *testing.T) {
	c := NewController(6.5)
	base := time.Unix(1_700_000_000, 0)
	c.SetHostMonitor(true, 18.5, 0) // Mac host monitor has 18.5ms output latency
	c.Tick(base)

	c.RegisterListener("mac-web", "Mac Chrome", "Chrome")
	c.RegisterListener("bt-phone", "Phone Bluetooth", "Safari")

	// Normal listener: raw = 6.5 + 2 + 40 + 20 = 68.5ms
	c.ReportTelemetry("mac-web", ListenerTelemetry{
		WsRttMs: 4, JitterBufferMs: 40, AudioOutputMs: 20, ClientGain: 1, Playing: true,
	}, base.Add(1*time.Second))
	c.ReportTelemetry("mac-web", ListenerTelemetry{
		WsRttMs: 4, JitterBufferMs: 40, AudioOutputMs: 20, ClientGain: 1, Playing: true,
	}, base.Add(2*time.Second))

	// Bluetooth phone: raw = 6.5 + 5 + 60 + 20 = 91.5ms, plus user pulls slider to -150ms for BT lag
	// Effective base = 91.5 - (-150) = 241.5ms -> desired = ceil((241.5+60)/50)*50 = 350ms
	btTel := ListenerTelemetry{
		WsRttMs: 10, JitterBufferMs: 60, AudioOutputMs: 20, UserOffsetMs: -150, ClientGain: 1, Playing: true,
	}
	c.ReportTelemetry("bt-phone", btTel, base.Add(2*time.Second))
	c.ReportTelemetry("bt-phone", btTel, base.Add(3*time.Second))

	for i := 3; i <= 15; i++ {
		now := base.Add(time.Duration(i) * time.Second)
		c.ReportTelemetry("bt-phone", btTel, now)
		c.Tick(now)
	}

	snap := c.Snapshot()
	if snap.DisplayTargetMs != 350.0 || math.Abs(snap.ActiveTargetMs-350.0) > 0.01 {
		t.Fatalf("expected converged target 350ms, got display=%v active=%v", snap.DisplayTargetMs, snap.ActiveTargetMs)
	}

	var macDelay, btDelay float64
	for _, l := range snap.Listeners {
		if l.ID == "mac-web" {
			macDelay = l.SendDelayMs
		}
		if l.ID == "bt-phone" {
			btDelay = l.SendDelayMs
		}
		if math.Abs(l.EstimatedSkewMs) > 0.01 {
			t.Fatalf("listener %s unexpected skew %v", l.ID, l.EstimatedSkewMs)
		}
	}

	// Host monitor should be delayed by 350 - 18.5 = 331.5ms
	if math.Abs(snap.HostDelayMs-331.5) > 0.01 {
		t.Fatalf("expected HostDelayMs=331.5, got %v", snap.HostDelayMs)
	}
	// Mac web delayed by 350 - 68.5 = 281.5ms
	if math.Abs(macDelay-281.5) > 0.01 {
		t.Fatalf("expected mac-web delay=281.5, got %v", macDelay)
	}
	// BT phone delayed by 350 - 241.5 = 108.5ms (150ms earlier than its uncalibrated path)
	if math.Abs(btDelay-108.5) > 0.01 {
		t.Fatalf("expected bt-phone delay=108.5, got %v", btDelay)
	}
}

func TestSyncControllerLowerHysteresisAfterDisconnect(t *testing.T) {
	c := NewController(6.5)
	base := time.Unix(1_700_000_000, 0)
	c.Tick(base)

	c.RegisterListener("slow", "Slow", "Safari")
	c.RegisterListener("fast", "Fast", "Chrome")

	slowTel := ListenerTelemetry{WsRttMs: 20, JitterBufferMs: 100, AudioOutputMs: 300, ClientGain: 1, Playing: true}
	fastTel := ListenerTelemetry{WsRttMs: 4, JitterBufferMs: 40, AudioOutputMs: 20, ClientGain: 1, Playing: true}

	for i := 1; i <= 8; i++ {
		now := base.Add(time.Duration(i) * time.Second)
		c.ReportTelemetry("slow", slowTel, now)
		c.ReportTelemetry("fast", fastTel, now)
		c.Tick(now)
	}
	if got := c.Snapshot().DisplayTargetMs; got != 500.0 {
		t.Fatalf("expected DisplayTargetMs=500.0 with slow device, got %v", got)
	}

	// Slow device leaves; fast device stays. Must hold for 30s before dropping target.
	c.RemoveListener("slow")
	for i := 9; i <= 35; i++ {
		now := base.Add(time.Duration(i) * time.Second)
		c.ReportTelemetry("fast", fastTel, now)
		snap := c.Tick(now)
		if snap.DisplayTargetMs != 500.0 {
			t.Fatalf("at %ds expected DisplayTargetMs to remain 500.0 during 30s lower hold, got %v", i, snap.DisplayTargetMs)
		}
	}

	// After 30s+ of fast-only path (desired = 250ms), target should drop to 250ms.
	for i := 36; i <= 42; i++ {
		now := base.Add(time.Duration(i) * time.Second)
		c.ReportTelemetry("fast", fastTel, now)
		c.Tick(now)
	}
	if got := c.Snapshot().DisplayTargetMs; got != 250.0 {
		t.Fatalf("expected DisplayTargetMs=250.0 after lower hold, got %v", got)
	}
}
