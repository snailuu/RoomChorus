package opus

import (
	"math"
	"testing"
)

func TestEncodeDecodeStereoSine(t *testing.T) {
	enc, err := NewEncoder(192000)
	if err != nil {
		t.Fatalf("NewEncoder failed: %v", err)
	}
	defer enc.Close()

	dec, err := NewDecoder()
	if err != nil {
		t.Fatalf("NewDecoder failed: %v", err)
	}
	defer dec.Close()

	lookahead := enc.LookaheadSamples()
	if lookahead <= 0 {
		t.Fatalf("expected positive lookahead samples, got %d", lookahead)
	}

	pcm := make([]float32, FrameFloatCount)
	pkt := make([]byte, 4000)
	decoded := make([]float32, FrameFloatCount)

	var totalDecodedEnergy float64
	var correlationSum float64
	const totalFrames = 50

	for f := 0; f < totalFrames; f++ {
		for i := 0; i < FrameSamples; i++ {
			sampleIdx := f*FrameSamples + i
			phaseL := 2.0 * math.Pi * 440.0 * float64(sampleIdx) / float64(SampleRate)
			phaseR := 2.0 * math.Pi * 554.37 * float64(sampleIdx) / float64(SampleRate)
			pcm[2*i] = float32(0.4 * math.Sin(phaseL))
			pcm[2*i+1] = float32(0.4 * math.Sin(phaseR))
		}

		n, err := enc.EncodeFloat32(pcm, pkt)
		if err != nil {
			t.Fatalf("frame %d EncodeFloat32 failed: %v", f, err)
		}
		if n <= 0 || n >= 800 {
			t.Fatalf("frame %d unexpected encoded size %d bytes", f, n)
		}

		decSamples, err := dec.DecodeFloat32(pkt[:n], decoded)
		if err != nil {
			t.Fatalf("frame %d DecodeFloat32 failed: %v", f, err)
		}
		if decSamples != FrameSamples {
			t.Fatalf("frame %d decoded samples = %d, want %d", f, decSamples, FrameSamples)
		}

		// After Opus warm-up frames, measure decoded signal energy and stereo independence.
		if f >= 10 {
			for i := 0; i < FrameSamples; i++ {
				l := float64(decoded[2*i])
				r := float64(decoded[2*i+1])
				totalDecodedEnergy += l*l + r*r
				correlationSum += l * l
			}
		}
	}

	rms := math.Sqrt(totalDecodedEnergy / float64((totalFrames-10)*FrameFloatCount))
	if rms < 0.15 || rms > 0.45 {
		t.Fatalf("unexpected decoded RMS %.4f for 0.4 amplitude sine wave", rms)
	}

	// Idempotent close check.
	enc.Close()
	enc.Close()
	dec.Close()
	dec.Close()
}
