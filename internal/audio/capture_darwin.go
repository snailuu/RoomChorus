package audio

/*
#cgo darwin CFLAGS: -Wall -Wno-deprecated-declarations
#cgo darwin LDFLAGS: -framework CoreAudio -framework AudioToolbox -framework CoreFoundation -framework Foundation
#include <stdlib.h>
#include "coreaudio_darwin.h"
*/
import "C"

import (
	"context"
	"errors"
	"fmt"
	"math"
	"sync"
	"sync/atomic"
	"time"
	"unsafe"

	"roomchorus/internal/opus"
)

// NowNs returns the unified monotonic clock timestamp in nanoseconds (CLOCK_UPTIME_RAW).
func NowNs() int64 {
	return int64(C.ss_now_ns())
}

// NowMs returns the unified monotonic clock timestamp in fractional milliseconds.
func NowMs() float64 {
	return float64(NowNs()) / 1e6
}

// ListDevices enumerates macOS CoreAudio input and output devices (excluding private tap aggregates).
func ListDevices() []DeviceInfo {
	const maxDevs = 128
	var buf [maxDevs]C.SSDeviceInfo
	n := int(C.ss_list_devices(&buf[0], C.int(maxDevs)))
	res := make([]DeviceInfo, 0, n)
	for i := 0; i < n; i++ {
		d := buf[i]
		res = append(res, DeviceInfo{
			ID:              uint32(d.id),
			UID:             C.GoString(&d.uid[0]),
			Name:            C.GoString(&d.name[0]),
			SampleRate:      float64(d.sample_rate),
			HasInput:        d.has_input != 0,
			HasOutput:       d.has_output != 0,
			IsDefaultInput:  d.is_default_input != 0,
			IsDefaultOutput: d.is_default_output != 0,
		})
	}
	return res
}

// SelfProcessObjectID returns the CoreAudio AudioObjectID for this process (used for tap echo exclusion).
func SelfProcessObjectID() uint32 {
	return uint32(C.ss_get_self_process_object_id())
}

// Engine manages macOS CoreAudio capture (Process Tap / input device / test tone) and synchronized HostMonitor playback.
type Engine struct {
	mu sync.Mutex

	capturing     bool
	mode          SourceMode
	captureCancel context.CancelFunc
	captureDone   chan struct{}

	monEnabled    bool
	monDeviceID   uint32
	monDeviceName string
	monLatencyMs  float64
	monDelayMs    float32
	monGain       float32
	monMuted      bool

	silentWarning atomic.Bool
	lastRms       atomic.Uint32
}

// NewEngine creates a new macOS CoreAudio engine.
func NewEngine() *Engine {
	return &Engine{
		monGain:    0.85,
		monDelayMs: 300.0,
	}
}

// StartCapture begins capturing 20ms 48kHz stereo PCM frames and delivers them to onFrame.
func (e *Engine) StartCapture(mode SourceMode, deviceID uint32, muteOriginal bool, clockDeviceID uint32, onFrame func(PCMBlock)) error {
	e.StopCapture()

	e.mu.Lock()
	defer e.mu.Unlock()

	var errBuf [256]C.char
	switch mode {
	case SourceSystem:
		muteFlag := 0
		if muteOriginal {
			muteFlag = 1
		}
		rc := C.ss_start_system_tap(C.int(muteFlag), C.uint32_t(clockDeviceID), &errBuf[0], C.int(len(errBuf)))
		if rc != 0 {
			return fmt.Errorf("system tap error: %s", C.GoString(&errBuf[0]))
		}
	case SourceInput:
		rc := C.ss_start_input_device(C.uint32_t(deviceID), &errBuf[0], C.int(len(errBuf)))
		if rc != 0 {
			return fmt.Errorf("input capture error: %s", C.GoString(&errBuf[0]))
		}
	case SourceTestTone:
		// Handled in pure Go below.
	default:
		return fmt.Errorf("unsupported capture mode: %q", mode)
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	e.capturing = true
	e.mode = mode
	e.captureCancel = cancel
	e.captureDone = done
	e.silentWarning.Store(false)
	e.lastRms.Store(0)

	go e.runCaptureLoop(ctx, done, mode, onFrame)
	return nil
}

func (e *Engine) runCaptureLoop(ctx context.Context, done chan struct{}, mode SourceMode, onFrame func(PCMBlock)) {
	defer close(done)

	if mode == SourceTestTone {
		RunTestToneLoop(ctx, func(blk PCMBlock) {
			e.lastRms.Store(math.Float32bits(blk.RmsLevel))
			if onFrame != nil {
				onFrame(blk)
			}
		})
		return
	}

	var seq uint64
	silentFrames := 0
	ticker := time.NewTicker(4 * time.Millisecond)
	defer ticker.Stop()

	buf := make([]float32, opus.FrameFloatCount)
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			for {
				var capNs C.int64_t
				n := int(C.ss_capture_read((*C.float)(unsafe.Pointer(&buf[0])), C.int(opus.FrameSamples), &capNs))
				if n <= 0 {
					break
				}
				frameCopy := make([]float32, opus.FrameFloatCount)
				copy(frameCopy, buf)
				rms := computeRMS(frameCopy)
				e.lastRms.Store(math.Float32bits(rms))

				if mode == SourceSystem {
					if rms < 1e-6 {
						silentFrames++
						if silentFrames >= 200 { // 4 seconds of absolute digital silence
							e.silentWarning.Store(true)
						}
					} else {
						silentFrames = 0
						e.silentWarning.Store(false)
					}
				}

				seq++
				if onFrame != nil {
					onFrame(PCMBlock{
						Samples:   frameCopy,
						CaptureNs: int64(capNs),
						Seq:       seq,
						RmsLevel:  rms,
					})
				}
			}
		}
	}
}

// StopCapture stops the active audio source and cleans up CoreAudio resources.
func (e *Engine) StopCapture() {
	e.mu.Lock()
	cancel := e.captureCancel
	done := e.captureDone
	wasCapturing := e.capturing
	mode := e.mode
	e.capturing = false
	e.captureCancel = nil
	e.captureDone = nil
	e.mu.Unlock()

	if cancel != nil {
		cancel()
	}
	if done != nil {
		<-done
	}
	if wasCapturing && mode != SourceTestTone {
		C.ss_stop_capture()
	}
	e.silentWarning.Store(false)
	e.lastRms.Store(0)
}

// StartMonitor starts synchronized local playback on the specified output device (0 = default output).
func (e *Engine) StartMonitor(outputDeviceID uint32) (float64, error) {
	e.mu.Lock()
	defer e.mu.Unlock()

	var latMs C.double
	var errBuf [256]C.char
	rc := C.ss_start_monitor(C.uint32_t(outputDeviceID), &latMs, &errBuf[0], C.int(len(errBuf)))
	if rc != 0 {
		return 0, errors.New(C.GoString(&errBuf[0]))
	}

	devName := "默认扬声器"
	for _, d := range ListDevices() {
		if (outputDeviceID != 0 && d.ID == outputDeviceID) || (outputDeviceID == 0 && d.IsDefaultOutput) {
			devName = d.Name
			outputDeviceID = d.ID
			break
		}
	}

	e.monEnabled = true
	e.monDeviceID = outputDeviceID
	e.monDeviceName = devName
	e.monLatencyMs = float64(latMs)
	C.ss_monitor_set_params(C.float(e.monDelayMs), C.float(e.monGain), boolToInt(e.monMuted))
	return e.monLatencyMs, nil
}

// StopMonitor stops local synchronized playback.
func (e *Engine) StopMonitor() {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.monEnabled {
		C.ss_stop_monitor()
		e.monEnabled = false
		e.monLatencyMs = 0
	}
}

// PushMonitor pushes a captured 48kHz stereo PCM frame to the HostMonitor ring buffer.
func (e *Engine) PushMonitor(blk PCMBlock) {
	if len(blk.Samples) != opus.FrameFloatCount {
		return
	}
	C.ss_monitor_push((*C.float)(unsafe.Pointer(&blk.Samples[0])), C.int(opus.FrameSamples), C.int64_t(blk.CaptureNs))
}

// SetMonitorParams updates host monitor delay, gain, and mute state lock-free.
func (e *Engine) SetMonitorParams(targetDelayMs float64, gain float64, muted bool) {
	e.mu.Lock()
	e.monDelayMs = float32(targetDelayMs)
	e.monGain = float32(gain)
	e.monMuted = muted
	e.mu.Unlock()

	C.ss_monitor_set_params(C.float(targetDelayMs), C.float(gain), boolToInt(muted))
}

// MonitorStatus returns a snapshot of the local host speaker monitor.
func (e *Engine) MonitorStatus() MonitorStatus {
	e.mu.Lock()
	enabled := e.monEnabled
	devID := e.monDeviceID
	devName := e.monDeviceName
	latMs := e.monLatencyMs
	targetDelay := float64(e.monDelayMs)
	e.mu.Unlock()

	var actualDelay C.double
	var underruns C.uint64_t
	C.ss_monitor_stats(&actualDelay, &underruns)

	return MonitorStatus{
		Enabled:         enabled,
		DeviceID:        devID,
		DeviceName:      devName,
		OutputLatencyMs: latMs,
		TargetDelayMs:   targetDelay,
		ActualDelayMs:   float64(actualDelay),
		Underruns:       uint64(underruns),
	}
}

// RmsLevel returns the most recent captured audio RMS amplitude (0..1).
func (e *Engine) RmsLevel() float32 {
	return math.Float32frombits(e.lastRms.Load())
}

// SilentWarning reports whether system tap has observed continuous absolute silence (>4s).
func (e *Engine) SilentWarning() bool {
	return e.silentWarning.Load()
}

// Close stops capture and monitor and releases all CoreAudio handles.
func (e *Engine) Close() {
	e.StopCapture()
	e.StopMonitor()
}

func boolToInt(b bool) C.int {
	if b {
		return 1
	}
	return 0
}

func computeRMS(pcm []float32) float32 {
	if len(pcm) == 0 {
		return 0
	}
	var sum float64
	for _, s := range pcm {
		v := float64(s)
		sum += v * v
	}
	rms := float32(math.Sqrt(sum / float64(len(pcm))))
	if rms > 1.0 {
		return 1.0
	}
	return rms
}
