package audio

// DeviceInfo describes an available macOS CoreAudio hardware or virtual audio device.
type DeviceInfo struct {
	ID              uint32  `json:"id"`
	UID             string  `json:"uid"`
	Name            string  `json:"name"`
	SampleRate      float64 `json:"sampleRate"`
	HasInput        bool    `json:"hasInput"`
	HasOutput       bool    `json:"hasOutput"`
	IsDefaultInput  bool    `json:"isDefaultInput"`
	IsDefaultOutput bool    `json:"isDefaultOutput"`
}

// SourceMode defines how the host captures broadcast audio.
type SourceMode string

const (
	SourceSystem   SourceMode = "system"
	SourceInput    SourceMode = "input"
	SourceTestTone SourceMode = "test-tone"
)

// PCMBlock holds one 20ms frame of 48kHz stereo interleaved float32 audio.
type PCMBlock struct {
	Samples   []float32
	CaptureNs int64
	Seq       uint64
	RmsLevel  float32
}

// MonitorStatus reports the state of synchronized local host speaker playback.
type MonitorStatus struct {
	Enabled         bool    `json:"enabled"`
	DeviceID        uint32  `json:"deviceId"`
	DeviceName      string  `json:"deviceName"`
	OutputLatencyMs float64 `json:"outputLatencyMs"`
	TargetDelayMs   float64 `json:"targetDelayMs"`
	ActualDelayMs   float64 `json:"actualDelayMs"`
	Underruns       uint64  `json:"underruns"`
}
