package rtc

import (
	"fmt"
	"sync"

	"roomchorus/internal/audio"
	"roomchorus/internal/opus"
	syncctrl "roomchorus/internal/sync"
)

// Hub manages shared/per-listener Opus encoders and routes each captured PCM frame to active peers and HostMonitor.
type Hub struct {
	mu       sync.Mutex
	engine   *audio.Engine
	syncCtrl *syncctrl.Controller

	bitrate     int
	hostVolume  float64
	hostMuted   bool
	monGain     float64
	monMuted    bool
	rtpTS       uint32
	peers       map[string]*ListenerPeer
	sharedEncs  map[syncctrl.ChannelMode]*opus.Encoder
	customEncs  map[string]*opus.Encoder
	scratchPCM  []float32
	scratchPkt  []byte
	lookaheadMs float64
}

// NewHub initializes an AudioHub wired to an Engine and SyncController.
func NewHub(engine *audio.Engine, syncCtrl *syncctrl.Controller, bitrate int) (*Hub, error) {
	if bitrate <= 0 {
		bitrate = opus.DefaultBitrate
	}
	stereoEnc, err := opus.NewEncoder(bitrate)
	if err != nil {
		return nil, err
	}
	lookahead := stereoEnc.LookaheadMs()
	if syncCtrl != nil {
		syncCtrl.SetOpusLookaheadMs(lookahead)
	}

	return &Hub{
		engine:      engine,
		syncCtrl:    syncCtrl,
		bitrate:     bitrate,
		hostVolume:  1.0,
		monGain:     0.85,
		peers:       make(map[string]*ListenerPeer),
		sharedEncs:  map[syncctrl.ChannelMode]*opus.Encoder{syncctrl.ChannelStereo: stereoEnc},
		customEncs:  make(map[string]*opus.Encoder),
		scratchPCM:  make([]float32, opus.FrameFloatCount),
		scratchPkt:  make([]byte, 4000),
		lookaheadMs: lookahead,
	}, nil
}

// AddPeer registers an active ListenerPeer to receive future encoded audio packets.
func (h *Hub) AddPeer(p *ListenerPeer) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if old, ok := h.peers[p.ID()]; ok && old != p {
		go old.Close()
	}
	h.peers[p.ID()] = p
}

// RemovePeer unregisters a ListenerPeer and frees any per-peer custom encoder.
func (h *Hub) RemovePeer(id string) {
	h.mu.Lock()
	p, ok := h.peers[id]
	if ok {
		delete(h.peers, id)
	}
	if enc, exists := h.customEncs[id]; exists {
		enc.Close()
		delete(h.customEncs, id)
	}
	h.mu.Unlock()

	if ok && p != nil {
		p.Close()
	}
}

// SetHostAudioConfig updates broadcast gain/mute, monitor gain/mute, and Opus bitrate.
func (h *Hub) SetHostAudioConfig(hostVolume float64, hostMuted bool, monGain float64, monMuted bool, bitrate int) error {
	h.mu.Lock()
	defer h.mu.Unlock()

	if hostVolume < 0 {
		hostVolume = 0
	}
	if hostVolume > 1 {
		hostVolume = 1
	}
	if monGain < 0 {
		monGain = 0
	}
	if monGain > 1 {
		monGain = 1
	}
	h.hostVolume = hostVolume
	h.hostMuted = hostMuted
	h.monGain = monGain
	h.monMuted = monMuted

	if bitrate > 0 && bitrate != h.bitrate {
		newStereo, err := opus.NewEncoder(bitrate)
		if err != nil {
			return fmt.Errorf("recreate opus encoder at %d bps: %w", bitrate, err)
		}
		for k, enc := range h.sharedEncs {
			enc.Close()
			delete(h.sharedEncs, k)
		}
		for k, enc := range h.customEncs {
			enc.Close()
			delete(h.customEncs, k)
		}
		h.sharedEncs[syncctrl.ChannelStereo] = newStereo
		h.bitrate = bitrate
		h.lookaheadMs = newStereo.LookaheadMs()
		if h.syncCtrl != nil {
			h.syncCtrl.SetOpusLookaheadMs(h.lookaheadMs)
		}
	}

	if h.engine != nil && h.syncCtrl != nil {
		h.engine.SetMonitorParams(h.syncCtrl.HostSendDelay(), h.monGain, h.monMuted)
	}
	return nil
}

// ClearAllQueues drops buffered frames across all active peers (used on source change).
func (h *Hub) ClearAllQueues() {
	h.mu.Lock()
	peers := make([]*ListenerPeer, 0, len(h.peers))
	for _, p := range h.peers {
		peers = append(peers, p)
	}
	h.mu.Unlock()
	for _, p := range peers {
		p.ClearQueue()
	}
}

// ProcessFrame feeds a captured 20ms PCM block to HostMonitor and encodes/enqueues it for all listeners.
func (h *Hub) ProcessFrame(blk audio.PCMBlock) {
	if len(blk.Samples) != opus.FrameFloatCount {
		return
	}

	h.mu.Lock()
	defer h.mu.Unlock()

	// Advance unified RTP timestamp monotonically by 960 samples (20ms @ 48kHz).
	curTS := h.rtpTS
	h.rtpTS += opus.FrameSamples

	// Feed unscaled PCM to HostMonitor so local hardware volume isn't attenuated twice.
	if h.engine != nil {
		var hostDelay float64 = syncctrl.DefaultTargetMs
		if h.syncCtrl != nil {
			hostDelay = h.syncCtrl.HostSendDelay()
		}
		h.engine.SetMonitorParams(hostDelay, h.monGain, h.monMuted)
		h.engine.PushMonitor(blk)
	}

	if len(h.peers) == 0 {
		return
	}

	hostGain := float32(h.hostVolume)
	if h.hostMuted {
		hostGain = 0
	}

	type encKey struct {
		mode syncctrl.ChannelMode
	}
	sharedPackets := make(map[encKey][]byte, 4)
	usedCustom := make(map[string]bool)

	for id, peer := range h.peers {
		mode := syncctrl.ChannelStereo
		clientGain := 1.0
		canSetVol := true
		if h.syncCtrl != nil {
			if _, m, cg, csv, ok := h.syncCtrl.ListenerSendDelay(id); ok {
				mode = m
				clientGain = cg
				canSetVol = csv
			}
		}

		// When browser cannot natively adjust <audio>.volume (e.g. iOS Safari),
		// apply listener volume gain cleanly inside a dedicated host-side Opus encoder.
		if !canSetVol && clientGain < 0.999 {
			usedCustom[id] = true
			enc := h.customEncs[id]
			if enc == nil {
				var err error
				enc, err = opus.NewEncoder(h.bitrate)
				if err != nil {
					continue
				}
				h.customEncs[id] = enc
			}
			applyChannelAndGain(blk.Samples, h.scratchPCM, mode, hostGain*float32(clientGain))
			n, err := enc.EncodeFloat32(h.scratchPCM, h.scratchPkt)
			if err != nil {
				continue
			}
			payload := make([]byte, n)
			copy(payload, h.scratchPkt[:n])
			peer.Enqueue(EncodedPacket{
				CaptureNs: blk.CaptureNs,
				RtpTS:     curTS,
				Payload:   payload,
			})
			continue
		}

		key := encKey{mode: mode}
		payload, ok := sharedPackets[key]
		if !ok {
			enc, err := h.getOrCreateSharedEncoderLocked(mode)
			if err != nil {
				continue
			}
			applyChannelAndGain(blk.Samples, h.scratchPCM, mode, hostGain)
			n, err := enc.EncodeFloat32(h.scratchPCM, h.scratchPkt)
			if err != nil {
				continue
			}
			payload = make([]byte, n)
			copy(payload, h.scratchPkt[:n])
			sharedPackets[key] = payload
		}

		peer.Enqueue(EncodedPacket{
			CaptureNs: blk.CaptureNs,
			RtpTS:     curTS,
			Payload:   payload,
		})
	}

	// Clean up custom encoders that are no longer needed (e.g. iOS user dragged volume back to 100%).
	for id, enc := range h.customEncs {
		if !usedCustom[id] {
			enc.Close()
			delete(h.customEncs, id)
		}
	}
}

// PeerStats returns sent and dropped RTP packet totals for each active peer.
func (h *Hub) PeerStats() map[string][2]uint64 {
	h.mu.Lock()
	defer h.mu.Unlock()
	out := make(map[string][2]uint64, len(h.peers))
	for id, p := range h.peers {
		s, d := p.Stats()
		out[id] = [2]uint64{s, d}
	}
	return out
}

// Close closes all peers and Opus encoders.
func (h *Hub) Close() {
	h.mu.Lock()
	peers := make([]*ListenerPeer, 0, len(h.peers))
	for id, p := range h.peers {
		peers = append(peers, p)
		delete(h.peers, id)
	}
	for k, enc := range h.sharedEncs {
		enc.Close()
		delete(h.sharedEncs, k)
	}
	for k, enc := range h.customEncs {
		enc.Close()
		delete(h.customEncs, k)
	}
	h.mu.Unlock()

	for _, p := range peers {
		p.Close()
	}
}

func (h *Hub) getOrCreateSharedEncoderLocked(mode syncctrl.ChannelMode) (*opus.Encoder, error) {
	if enc, ok := h.sharedEncs[mode]; ok {
		return enc, nil
	}
	enc, err := opus.NewEncoder(h.bitrate)
	if err != nil {
		return nil, err
	}
	h.sharedEncs[mode] = enc
	return enc, nil
}

func applyChannelAndGain(src, dst []float32, mode syncctrl.ChannelMode, gain float32) {
	frames := len(src) / 2
	for i := 0; i < frames; i++ {
		l := src[2*i]
		r := src[2*i+1]
		switch mode {
		case syncctrl.ChannelLeft:
			v := l * gain
			dst[2*i] = v
			dst[2*i+1] = v
		case syncctrl.ChannelRight:
			v := r * gain
			dst[2*i] = v
			dst[2*i+1] = v
		case syncctrl.ChannelMono:
			v := 0.5 * (l + r) * gain
			dst[2*i] = v
			dst[2*i+1] = v
		default:
			dst[2*i] = l * gain
			dst[2*i+1] = r * gain
		}
	}
}
