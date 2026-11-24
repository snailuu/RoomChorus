package rtc

import (
	"fmt"
	"net"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/pion/rtp"
	"github.com/pion/webrtc/v4"

	"roomchorus/internal/audio"
	syncctrl "roomchorus/internal/sync"
)

const (
	MaxQueueFrames     = 120 // 2.4 seconds @ 20ms per frame
	StaleOverdueNs     = 400_000_000
	SenderTickInterval = 5 * time.Millisecond
)

// EncodedPacket carries a single 20ms Opus frame tagged with monotonic capture time and 48kHz RTP timestamp.
type EncodedPacket struct {
	CaptureNs int64
	RtpTS     uint32
	Payload   []byte
}

// PeerConfig configures a single WebRTC listener connection.
type PeerConfig struct {
	ID             string
	AllowedIP      string
	SyncCtrl       *syncctrl.Controller
	OnICECandidate func(webrtc.ICECandidateInit)
	OnStateChange  func(webrtc.PeerConnectionState)
}

// ListenerPeer manages one browser's WebRTC PeerConnection, audio track, and bounded delay queue.
type ListenerPeer struct {
	id       string
	pc       *webrtc.PeerConnection
	track    *webrtc.TrackLocalStaticRTP
	syncCtrl *syncctrl.Controller

	mu                sync.Mutex
	queue             []EncodedPacket
	seqNum            uint16
	remoteSet         bool
	pendingCandidates []webrtc.ICECandidateInit
	closed            bool

	droppedFrames atomic.Uint64
	sentFrames    atomic.Uint64

	stopCh chan struct{}
	doneCh chan struct{}
}

// NewListenerPeer creates a LAN-only WebRTC peer configured for 48kHz stereo Opus output.
func NewListenerPeer(cfg PeerConfig) (*ListenerPeer, error) {
	me := &webrtc.MediaEngine{}
	if err := me.RegisterCodec(webrtc.RTPCodecParameters{
		RTPCodecCapability: webrtc.RTPCodecCapability{
			MimeType:    webrtc.MimeTypeOpus,
			ClockRate:   48000,
			Channels:    2,
			SDPFmtpLine: "minptime=10;useinbandfec=1",
		},
		PayloadType: 111,
	}, webrtc.RTPCodecTypeAudio); err != nil {
		return nil, fmt.Errorf("register opus codec: %w", err)
	}

	se := webrtc.SettingEngine{}
	se.SetIncludeLoopbackCandidate(true)
	se.SetNetworkTypes([]webrtc.NetworkType{webrtc.NetworkTypeUDP4, webrtc.NetworkTypeTCP4})
	se.SetICETimeouts(5*time.Second, 15*time.Second, 2*time.Second)
	if cfg.AllowedIP != "" {
		allowed := cfg.AllowedIP
		se.SetIPFilter(func(ip net.IP) bool {
			return ip.IsLoopback() || ip.String() == allowed
		})
	}

	api := webrtc.NewAPI(
		webrtc.WithMediaEngine(me),
		webrtc.WithSettingEngine(se),
	)

	pc, err := api.NewPeerConnection(webrtc.Configuration{ICEServers: nil})
	if err != nil {
		return nil, fmt.Errorf("create peer connection: %w", err)
	}

	track, err := webrtc.NewTrackLocalStaticRTP(
		webrtc.RTPCodecCapability{
			MimeType:  webrtc.MimeTypeOpus,
			ClockRate: 48000,
			Channels:  2,
		},
		"roomchorus-audio",
		"roomchorus-stream",
	)
	if err != nil {
		_ = pc.Close()
		return nil, fmt.Errorf("create opus track: %w", err)
	}

	if _, err := pc.AddTrack(track); err != nil {
		_ = pc.Close()
		return nil, fmt.Errorf("add opus track: %w", err)
	}

	p := &ListenerPeer{
		id:       cfg.ID,
		pc:       pc,
		track:    track,
		syncCtrl: cfg.SyncCtrl,
		queue:    make([]EncodedPacket, 0, MaxQueueFrames),
		stopCh:   make(chan struct{}),
		doneCh:   make(chan struct{}),
	}

	pc.OnICECandidate(func(c *webrtc.ICECandidate) {
		if c != nil && cfg.OnICECandidate != nil {
			cfg.OnICECandidate(c.ToJSON())
		}
	})

	pc.OnConnectionStateChange(func(state webrtc.PeerConnectionState) {
		if cfg.OnStateChange != nil {
			cfg.OnStateChange(state)
		}
	})

	go p.runSender()
	return p, nil
}

// ID returns the listener identifier.
func (p *ListenerPeer) ID() string {
	return p.id
}

// HandleOffer sets the remote SDP offer, creates a stereo-enabled SDP answer, and flushes queued ICE candidates.
func (p *ListenerPeer) HandleOffer(offer webrtc.SessionDescription) (webrtc.SessionDescription, error) {
	if err := p.pc.SetRemoteDescription(offer); err != nil {
		return webrtc.SessionDescription{}, fmt.Errorf("set remote description: %w", err)
	}

	p.mu.Lock()
	p.remoteSet = true
	pending := p.pendingCandidates
	p.pendingCandidates = nil
	p.mu.Unlock()

	for _, c := range pending {
		_ = p.pc.AddICECandidate(c)
	}

	answer, err := p.pc.CreateAnswer(nil)
	if err != nil {
		return webrtc.SessionDescription{}, fmt.Errorf("create answer: %w", err)
	}

	gatherDone := webrtc.GatheringCompletePromise(p.pc)
	if err := p.pc.SetLocalDescription(answer); err != nil {
		return webrtc.SessionDescription{}, fmt.Errorf("set local description: %w", err)
	}

	select {
	case <-gatherDone:
	case <-time.After(250 * time.Millisecond):
	}

	local := p.pc.LocalDescription()
	if local == nil {
		answer.SDP = EnsureStereoOpusSDP(answer.SDP)
		return answer, nil
	}
	out := *local
	out.SDP = EnsureStereoOpusSDP(out.SDP)
	return out, nil
}

// AddICECandidate adds a trickle ICE candidate, queueing it if SetRemoteDescription has not run yet.
func (p *ListenerPeer) AddICECandidate(cand webrtc.ICECandidateInit) error {
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		return nil
	}
	if !p.remoteSet {
		p.pendingCandidates = append(p.pendingCandidates, cand)
		p.mu.Unlock()
		return nil
	}
	p.mu.Unlock()
	return p.pc.AddICECandidate(cand)
}

// Enqueue appends an encoded 20ms Opus packet into this peer's bounded delay queue.
func (p *ListenerPeer) Enqueue(pkt EncodedPacket) {
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		return
	}
	if len(p.queue) >= MaxQueueFrames {
		// Drop oldest frame if queue hits absolute cap.
		copy(p.queue, p.queue[1:])
		p.queue = p.queue[:len(p.queue)-1]
		p.droppedFrames.Add(1)
	}
	p.queue = append(p.queue, pkt)
	p.mu.Unlock()
}

// ClearQueue drops all pending packets (for example when source restarts).
func (p *ListenerPeer) ClearQueue() {
	p.mu.Lock()
	p.queue = p.queue[:0]
	p.mu.Unlock()
}

// Stats returns sent and dropped frame counts for diagnostics.
func (p *ListenerPeer) Stats() (sent uint64, dropped uint64) {
	return p.sentFrames.Load(), p.droppedFrames.Load()
}

// Close stops the sender loop and closes the underlying PeerConnection.
func (p *ListenerPeer) Close() {
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		return
	}
	p.closed = true
	close(p.stopCh)
	p.mu.Unlock()

	<-p.doneCh
	_ = p.pc.Close()
}

func (p *ListenerPeer) runSender() {
	defer close(p.doneCh)
	ticker := time.NewTicker(SenderTickInterval)
	defer ticker.Stop()

	for {
		select {
		case <-p.stopCh:
			return
		case <-ticker.C:
			p.flushDuePackets(audio.NowNs())
		}
	}
}

func (p *ListenerPeer) flushDuePackets(nowNs int64) {
	delayMs := syncctrl.DefaultTargetMs
	if p.syncCtrl != nil {
		if d, _, _, _, ok := p.syncCtrl.ListenerSendDelay(p.id); ok {
			delayMs = d
		}
	}
	delayNs := int64(delayMs * 1_000_000.0)

	var ready []EncodedPacket
	var startSeq uint16

	p.mu.Lock()
	if p.closed || len(p.queue) == 0 {
		p.mu.Unlock()
		return
	}

	// Drop packets that are significantly overdue (> 120ms past due) to prevent unbounded backlog.
	cut := 0
	for cut < len(p.queue) {
		dueNs := p.queue[cut].CaptureNs + delayNs
		if nowNs-dueNs > StaleOverdueNs {
			cut++
			p.droppedFrames.Add(1)
			continue
		}
		break
	}
	if cut > 0 {
		n := copy(p.queue, p.queue[cut:])
		p.queue = p.queue[:n]
	}

	readyCount := 0
	for readyCount < len(p.queue) {
		dueNs := p.queue[readyCount].CaptureNs + delayNs
		if nowNs >= dueNs {
			readyCount++
		} else {
			break
		}
	}

	if readyCount > 0 {
		ready = make([]EncodedPacket, readyCount)
		copy(ready, p.queue[:readyCount])
		n := copy(p.queue, p.queue[readyCount:])
		p.queue = p.queue[:n]
		startSeq = p.seqNum
		p.seqNum += uint16(readyCount)
	}
	p.mu.Unlock()

	for i, pkt := range ready {
		rtpPkt := &rtp.Packet{
			Header: rtp.Header{
				Version:        2,
				PayloadType:    111,
				SequenceNumber: startSeq + uint16(i),
				Timestamp:      pkt.RtpTS,
			},
			Payload: pkt.Payload,
		}
		if err := p.track.WriteRTP(rtpPkt); err == nil {
			p.sentFrames.Add(1)
		}
	}
}

var opusRtpmapRe = regexp.MustCompile(`(?m)^a=rtpmap:(\d+)\s+opus/48000/2\r?$`)

// EnsureStereoOpusSDP ensures the Opus a=fmtp line advertises stereo=1 and sprop-stereo=1.
func EnsureStereoOpusSDP(sdp string) string {
	m := opusRtpmapRe.FindStringSubmatch(sdp)
	if len(m) < 2 {
		return sdp
	}
	pt := m[1]
	lineEnd := "\n"
	if strings.Contains(sdp, "\r\n") {
		lineEnd = "\r\n"
	}

	fmtpPrefix := "a=fmtp:" + pt + " "
	lines := strings.Split(strings.ReplaceAll(sdp, "\r\n", "\n"), "\n")
	foundFmtp := false
	for i, line := range lines {
		if strings.HasPrefix(line, fmtpPrefix) {
			foundFmtp = true
			params := strings.TrimPrefix(line, fmtpPrefix)
			params = ensureFmtpParam(params, "minptime", "10")
			params = ensureFmtpParam(params, "useinbandfec", "1")
			params = ensureFmtpParam(params, "stereo", "1")
			params = ensureFmtpParam(params, "sprop-stereo", "1")
			params = ensureFmtpParam(params, "maxaveragebitrate", "256000")
			lines[i] = fmtpPrefix + params
			break
		}
	}

	if !foundFmtp {
		rtpmapLine := "a=rtpmap:" + pt + " "
		outLines := make([]string, 0, len(lines)+1)
		for _, line := range lines {
			outLines = append(outLines, line)
			if strings.HasPrefix(line, rtpmapLine) {
				outLines = append(outLines, fmtpPrefix+"minptime=10;useinbandfec=1;stereo=1;sprop-stereo=1;maxaveragebitrate=256000")
			}
		}
		lines = outLines
	}

	return strings.Join(lines, lineEnd)
}

func ensureFmtpParam(fmtp, key, val string) string {
	parts := strings.Split(fmtp, ";")
	cleaned := make([]string, 0, len(parts)+1)
	replaced := false
	for _, raw := range parts {
		p := strings.TrimSpace(raw)
		if p == "" {
			continue
		}
		if strings.HasPrefix(p, key+"=") {
			cleaned = append(cleaned, key+"="+val)
			replaced = true
		} else {
			cleaned = append(cleaned, p)
		}
	}
	if !replaced {
		cleaned = append(cleaned, key+"="+val)
	}
	return strings.Join(cleaned, ";")
}
