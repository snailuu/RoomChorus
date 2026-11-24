package audio

import (
	"context"
	"math"
	"time"

	"roomchorus/internal/opus"
)

// RunTestToneLoop emits 20ms 48kHz stereo calibration frames paced against NowNs.
// Every 1.0 second, it produces a crisp 30ms click/beep (880Hz on Left, 1100Hz on Right)
// accompanied by a subtle continuous 220Hz warm drone so Opus/JitterBuffer stay active
// and listeners can aurally verify multi-device sync and left/right channel routing.
func RunTestToneLoop(ctx context.Context, onFrame func(PCMBlock)) {
	const frameNs = int64(opus.FrameDurationMs) * 1_000_000
	startNs := NowNs()
	var seq uint64
	var sampleCursor int64

	for {
		select {
		case <-ctx.Done():
			return
		default:
		}

		targetWakeNs := startNs + int64(seq+1)*frameNs
		now := NowNs()
		if sleepNs := targetWakeNs - now; sleepNs > 500_000 {
			timer := time.NewTimer(time.Duration(sleepNs) * time.Nanosecond)
			select {
			case <-ctx.Done():
				timer.Stop()
				return
			case <-timer.C:
			}
		}

		frameNsStart := startNs + int64(seq)*frameNs
		samples := make([]float32, opus.FrameFloatCount)

		for i := 0; i < opus.FrameSamples; i++ {
			idx := sampleCursor + int64(i)
			posInSec := idx % int64(opus.SampleRate)
			t := float64(idx) / float64(opus.SampleRate)

			// Gentle background tone (-34dB) keeps WebRTC audio stream warm without annoyance.
			bg := 0.02 * math.Sin(2.0*math.Pi*220.0*t)

			var l, r float64
			if posInSec < 1440 { // 30ms calibration pulse every 1 second
				env := 1.0
				if posInSec < 120 {
					env = float64(posInSec) / 120.0
				} else if posInSec > 1320 {
					env = float64(1440-posInSec) / 120.0
				}
				l = bg + 0.35*env*math.Sin(2.0*math.Pi*880.0*t)
				r = bg + 0.35*env*math.Sin(2.0*math.Pi*1100.0*t)
			} else {
				l = bg
				r = bg
			}
			samples[2*i] = float32(l)
			samples[2*i+1] = float32(r)
		}

		sampleCursor += int64(opus.FrameSamples)
		seq++
		if onFrame != nil {
			onFrame(PCMBlock{
				Samples:   samples,
				CaptureNs: frameNsStart,
				Seq:       seq,
				RmsLevel:  computeRMS(samples),
			})
		}
	}
}
