package audio

import (
	"context"
	"testing"
	"time"

	"roomchorus/internal/opus"
)

func TestListDevicesAndSelfProcessObjectID(t *testing.T) {
	t0 := NowNs()
	time.Sleep(10 * time.Millisecond)
	t1 := NowNs()
	if t1 <= t0 {
		t.Fatalf("NowNs not monotonic: t0=%d t1=%d", t0, t1)
	}

	devs := ListDevices()
	if len(devs) == 0 {
		t.Fatal("expected at least one CoreAudio device on macOS")
	}

	var hasOut bool
	for _, d := range devs {
		if d.HasOutput {
			hasOut = true
		}
		if d.Name == "" {
			t.Fatalf("device %d has empty name", d.ID)
		}
	}
	if !hasOut {
		t.Log("warning: no CoreAudio output device reported in this environment")
	}

	procObj := SelfProcessObjectID()
	if procObj == 0 {
		t.Fatal("expected non-zero CoreAudio self AudioObjectID for tap echo exclusion")
	}
}

func TestTestToneLoop(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	ch := make(chan PCMBlock, 16)
	go RunTestToneLoop(ctx, func(blk PCMBlock) {
		select {
		case ch <- blk:
		default:
		}
	})

	var blocks []PCMBlock
	timeout := time.After(2 * time.Second)
	for len(blocks) < 5 {
		select {
		case b := <-ch:
			blocks = append(blocks, b)
		case <-timeout:
			t.Fatalf("timed out waiting for test tone blocks, got %d", len(blocks))
		}
	}
	cancel()

	for i, b := range blocks {
		if len(b.Samples) != opus.FrameFloatCount {
			t.Fatalf("block %d len = %d, want %d", i, len(b.Samples), opus.FrameFloatCount)
		}
		if b.RmsLevel <= 0 {
			t.Fatalf("block %d expected non-zero RMS, got %f", i, b.RmsLevel)
		}
		if i > 0 && b.CaptureNs != blocks[i-1].CaptureNs+int64(opus.FrameDurationMs)*1_000_000 {
			t.Fatalf("block %d timestamp step mismatch: prev=%d cur=%d", i, blocks[i-1].CaptureNs, b.CaptureNs)
		}
	}
}
