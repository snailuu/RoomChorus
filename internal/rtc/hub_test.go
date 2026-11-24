package rtc

import (
	"math"
	"strings"
	"testing"

	"roomchorus/internal/audio"
	syncctrl "roomchorus/internal/sync"
)

func TestApplyChannelAndGain(t *testing.T) {
	src := []float32{0.8, 0.2, -0.4, 0.6}
	dst := make([]float32, len(src))

	applyChannelAndGain(src, dst, syncctrl.ChannelStereo, 0.5)
	assertFloatSlice(t, dst, []float32{0.4, 0.1, -0.2, 0.3})

	applyChannelAndGain(src, dst, syncctrl.ChannelLeft, 1.0)
	assertFloatSlice(t, dst, []float32{0.8, 0.8, -0.4, -0.4})

	applyChannelAndGain(src, dst, syncctrl.ChannelRight, 1.0)
	assertFloatSlice(t, dst, []float32{0.2, 0.2, 0.6, 0.6})

	applyChannelAndGain(src, dst, syncctrl.ChannelMono, 0.5)
	assertFloatSlice(t, dst, []float32{0.25, 0.25, 0.05, 0.05})
}

func TestEnsureStereoOpusSDP(t *testing.T) {
	in := strings.Join([]string{
		"v=0",
		"m=audio 9 UDP/TLS/RTP/SAVPF 111",
		"a=rtpmap:111 opus/48000/2",
		"a=fmtp:111 minptime=10;useinbandfec=1",
	}, "\r\n")

	out := EnsureStereoOpusSDP(in)
	for _, want := range []string{"stereo=1", "sprop-stereo=1", "maxaveragebitrate=256000"} {
		if !strings.Contains(out, want) {
			t.Fatalf("expected SDP to contain %q, got:\n%s", want, out)
		}
	}
}

func TestListenerPeerDelayQueueFlushAndOverdueDrop(t *testing.T) {
	ctrl := syncctrl.NewController(6.5)
	ctrl.RegisterListener("p1", "Test", "Chrome")
	ctrl.UpdatePrefs("p1", syncctrl.ChannelStereo, 0, 1.0, nil, "")
	// Default active target is 300ms, effective base = 0 => sendDelayMs = 300ms

	peer, err := NewListenerPeer(PeerConfig{
		ID:       "p1",
		SyncCtrl: ctrl,
	})
	if err != nil {
		t.Fatalf("NewListenerPeer failed: %v", err)
	}
	defer peer.Close()

	baseNs := audio.NowNs()
	// Enqueue an overdue packet (captured 800ms ago -> 500ms past 300ms target -> >400ms overdue)
	peer.Enqueue(EncodedPacket{CaptureNs: baseNs - 800_000_000, RtpTS: 960, Payload: []byte{1, 2, 3}})
	// Enqueue a packet that is exactly due now (captured 310ms ago)
	peer.Enqueue(EncodedPacket{CaptureNs: baseNs - 310_000_000, RtpTS: 1920, Payload: []byte{1, 2, 3}})
	// Enqueue a future packet (captured now -> due in +300ms)
	peer.Enqueue(EncodedPacket{CaptureNs: baseNs, RtpTS: 2880, Payload: []byte{1, 2, 3}})

	peer.flushDuePackets(baseNs)

	if peer.droppedFrames.Load() != 1 {
		t.Fatalf("expected 1 overdue frame dropped, got %d", peer.droppedFrames.Load())
	}

	peer.mu.Lock()
	remaining := len(peer.queue)
	var remTS uint32
	if remaining == 1 {
		remTS = peer.queue[0].RtpTS
	}
	peer.mu.Unlock()

	if remaining != 1 || remTS != 2880 {
		t.Fatalf("expected 1 future packet (ts=2880) left in queue, got remaining=%d ts=%d", remaining, remTS)
	}
}

func assertFloatSlice(t *testing.T, got, want []float32) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("len=%d, want %d", len(got), len(want))
	}
	for i := range got {
		if math.Abs(float64(got[i]-want[i])) > 1e-5 {
			t.Fatalf("idx %d got %v, want %v", i, got[i], want[i])
		}
	}
}
