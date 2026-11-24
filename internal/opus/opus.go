package opus

/*
#cgo pkg-config: opus
#include <opus.h>

static int ss_opus_enc_set_bitrate(OpusEncoder *enc, opus_int32 bitrate) {
	return opus_encoder_ctl(enc, OPUS_SET_BITRATE(bitrate));
}

static int ss_opus_enc_set_signal(OpusEncoder *enc, opus_int32 signal) {
	return opus_encoder_ctl(enc, OPUS_SET_SIGNAL(signal));
}

static int ss_opus_enc_set_complexity(OpusEncoder *enc, opus_int32 complexity) {
	return opus_encoder_ctl(enc, OPUS_SET_COMPLEXITY(complexity));
}

static int ss_opus_enc_set_inband_fec(OpusEncoder *enc, opus_int32 fec) {
	return opus_encoder_ctl(enc, OPUS_SET_INBAND_FEC(fec));
}

static int ss_opus_enc_set_dtx(OpusEncoder *enc, opus_int32 dtx) {
	return opus_encoder_ctl(enc, OPUS_SET_DTX(dtx));
}

static int ss_opus_enc_get_lookahead(OpusEncoder *enc, opus_int32 *out) {
	return opus_encoder_ctl(enc, OPUS_GET_LOOKAHEAD(out));
}
*/
import "C"

import (
	"errors"
	"fmt"
	"sync"
)

const (
	SampleRate      = 48000
	Channels        = 2
	FrameDurationMs = 20
	FrameSamples    = 960
	FrameFloatCount = FrameSamples * Channels
	DefaultBitrate  = 192000
	MinBitrate      = 64000
	MaxBitrate      = 510000
)

// Encoder wraps libopus OpusEncoder for 48kHz stereo 20ms music frames.
type Encoder struct {
	mu  sync.Mutex
	enc *C.OpusEncoder
}

// NewEncoder creates an Opus encoder configured for stereo music streaming.
func NewEncoder(bitrate int) (*Encoder, error) {
	if bitrate <= 0 {
		bitrate = DefaultBitrate
	}
	if bitrate < MinBitrate {
		bitrate = MinBitrate
	}
	if bitrate > MaxBitrate {
		bitrate = MaxBitrate
	}

	var errCode C.int
	enc := C.opus_encoder_create(
		C.opus_int32(SampleRate),
		C.int(Channels),
		C.OPUS_APPLICATION_AUDIO,
		&errCode,
	)
	if errCode != C.OPUS_OK || enc == nil {
		return nil, fmt.Errorf("opus_encoder_create failed: %s (%d)", C.GoString(C.opus_strerror(errCode)), int(errCode))
	}

	if rc := C.ss_opus_enc_set_bitrate(enc, C.opus_int32(bitrate)); rc != C.OPUS_OK {
		C.opus_encoder_destroy(enc)
		return nil, fmt.Errorf("opus set bitrate failed: %s", C.GoString(C.opus_strerror(rc)))
	}
	if rc := C.ss_opus_enc_set_signal(enc, C.OPUS_SIGNAL_MUSIC); rc != C.OPUS_OK {
		C.opus_encoder_destroy(enc)
		return nil, fmt.Errorf("opus set signal failed: %s", C.GoString(C.opus_strerror(rc)))
	}
	if rc := C.ss_opus_enc_set_complexity(enc, 8); rc != C.OPUS_OK {
		C.opus_encoder_destroy(enc)
		return nil, fmt.Errorf("opus set complexity failed: %s", C.GoString(C.opus_strerror(rc)))
	}
	if rc := C.ss_opus_enc_set_inband_fec(enc, 1); rc != C.OPUS_OK {
		C.opus_encoder_destroy(enc)
		return nil, fmt.Errorf("opus set inband fec failed: %s", C.GoString(C.opus_strerror(rc)))
	}
	if rc := C.ss_opus_enc_set_dtx(enc, 0); rc != C.OPUS_OK {
		C.opus_encoder_destroy(enc)
		return nil, fmt.Errorf("opus set dtx failed: %s", C.GoString(C.opus_strerror(rc)))
	}

	return &Encoder{enc: enc}, nil
}

// LookaheadSamples returns the algorithmic lookahead of the encoder in samples at 48kHz.
func (e *Encoder) LookaheadSamples() int {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.enc == nil {
		return 0
	}
	var la C.opus_int32
	if rc := C.ss_opus_enc_get_lookahead(e.enc, &la); rc != C.OPUS_OK {
		return 0
	}
	return int(la)
}

// LookaheadMs returns the encoder algorithmic lookahead in milliseconds.
func (e *Encoder) LookaheadMs() float64 {
	return float64(e.LookaheadSamples()) * 1000.0 / float64(SampleRate)
}

// EncodeFloat32 encodes one interleaved 48kHz stereo 20ms PCM frame into out.
func (e *Encoder) EncodeFloat32(pcm []float32, out []byte) (int, error) {
	if len(pcm) != FrameFloatCount {
		return 0, fmt.Errorf("invalid pcm length: got %d, want %d", len(pcm), FrameFloatCount)
	}
	if len(out) == 0 {
		return 0, errors.New("output buffer is empty")
	}

	e.mu.Lock()
	defer e.mu.Unlock()
	if e.enc == nil {
		return 0, errors.New("encoder is closed")
	}

	n := C.opus_encode_float(
		e.enc,
		(*C.float)(&pcm[0]),
		C.int(FrameSamples),
		(*C.uchar)(&out[0]),
		C.opus_int32(len(out)),
	)
	if n < 0 {
		return 0, fmt.Errorf("opus_encode_float failed: %s (%d)", C.GoString(C.opus_strerror(C.int(n))), int(n))
	}
	return int(n), nil
}

// Close frees the underlying libopus encoder. Safe to call multiple times.
func (e *Encoder) Close() {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.enc != nil {
		C.opus_encoder_destroy(e.enc)
		e.enc = nil
	}
}

// Decoder wraps libopus OpusDecoder for 48kHz stereo verification and testing.
type Decoder struct {
	mu  sync.Mutex
	dec *C.OpusDecoder
}

// NewDecoder creates a 48kHz stereo Opus decoder.
func NewDecoder() (*Decoder, error) {
	var errCode C.int
	dec := C.opus_decoder_create(
		C.opus_int32(SampleRate),
		C.int(Channels),
		&errCode,
	)
	if errCode != C.OPUS_OK || dec == nil {
		return nil, fmt.Errorf("opus_decoder_create failed: %s (%d)", C.GoString(C.opus_strerror(errCode)), int(errCode))
	}
	return &Decoder{dec: dec}, nil
}

// DecodeFloat32 decodes an Opus packet into interleaved 48kHz stereo float32 PCM.
func (d *Decoder) DecodeFloat32(packet []byte, out []float32) (int, error) {
	if len(out) < FrameFloatCount {
		return 0, fmt.Errorf("output slice too small: got %d, want >= %d", len(out), FrameFloatCount)
	}
	if len(packet) == 0 {
		return 0, errors.New("empty opus packet")
	}

	d.mu.Lock()
	defer d.mu.Unlock()
	if d.dec == nil {
		return 0, errors.New("decoder is closed")
	}

	n := C.opus_decode_float(
		d.dec,
		(*C.uchar)(&packet[0]),
		C.opus_int32(len(packet)),
		(*C.float)(&out[0]),
		C.int(len(out)/Channels),
		0,
	)
	if n < 0 {
		return 0, fmt.Errorf("opus_decode_float failed: %s (%d)", C.GoString(C.opus_strerror(C.int(n))), int(n))
	}
	return int(n), nil
}

// Close frees the underlying libopus decoder. Safe to call multiple times.
func (d *Decoder) Close() {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.dec != nil {
		C.opus_decoder_destroy(d.dec)
		d.dec = nil
	}
}
