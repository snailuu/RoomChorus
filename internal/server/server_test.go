package server

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/pion/webrtc/v4"

	syncctrl "roomchorus/internal/sync"
)

func TestEndToEndWebRTCOpusAndSync(t *testing.T) {
	srv, err := New(Config{
		Port:                  0,
		DisableMonitorDefault: true,
	})
	if err != nil {
		t.Fatalf("New server failed: %v", err)
	}
	if err := srv.Start(); err != nil {
		t.Fatalf("Start server failed: %v", err)
	}
	defer func() {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		_ = srv.Close(ctx)
	}()

	if err := srv.StartBroadcast("test-tone"); err != nil {
		t.Fatalf("StartBroadcast(test-tone) failed: %v", err)
	}

	baseURL := fmt.Sprintf("http://127.0.0.1:%d", srv.Port())

	// 1. Verify HTTP endpoints
	for _, path := range []string{"/", "/host", "/assets/app.css", "/assets/host.js", "/assets/listener.js"} {
		resp, err := http.Get(baseURL + path)
		if err != nil {
			t.Fatalf("GET %s failed: %v", path, err)
		}
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("GET %s status = %d, want 200", path, resp.StatusCode)
		}
	}

	qrResp, err := http.Get(baseURL + "/api/qr.png?mode=ipv4")
	if err != nil {
		t.Fatalf("GET /api/qr.png failed: %v", err)
	}
	var qrBuf bytes.Buffer
	_, _ = qrBuf.ReadFrom(qrResp.Body)
	_ = qrResp.Body.Close()
	if qrResp.StatusCode != http.StatusOK || !bytes.HasPrefix(qrBuf.Bytes(), []byte{0x89, 'P', 'N', 'G'}) {
		t.Fatalf("invalid QR response: status=%d len=%d", qrResp.StatusCode, qrBuf.Len())
	}

	stateResp, err := http.Get(baseURL + "/api/host/state")
	if err != nil {
		t.Fatalf("GET /api/host/state failed: %v", err)
	}
	var hostState HostStatePayload
	if err := json.NewDecoder(stateResp.Body).Decode(&hostState); err != nil {
		_ = stateResp.Body.Close()
		t.Fatalf("decode host state failed: %v", err)
	}
	_ = stateResp.Body.Close()
	if !hostState.Config.Broadcasting || len(hostState.Devices) == 0 {
		t.Fatalf("expected broadcasting=true and non-empty devices, got broadcasting=%v devices=%d", hostState.Config.Broadcasting, len(hostState.Devices))
	}

	// 2. Verify security boundaries: non-loopback /host -> 403, bad token /ws/listen -> 401
	reqRemote := httptest.NewRequest(http.MethodGet, "/host", nil)
	reqRemote.RemoteAddr = "192.168.1.99:55555"
	recRemote := httptest.NewRecorder()
	srv.handleHostPage(recRemote, reqRemote)
	if recRemote.Code != http.StatusForbidden {
		t.Fatalf("non-loopback /host status = %d, want 403", recRemote.Code)
	}

	wsBadURL := fmt.Sprintf("ws://127.0.0.1:%d/ws/listen?k=wrongtoken", srv.Port())
	_, badResp, err := websocket.DefaultDialer.Dial(wsBadURL, nil)
	if err == nil {
		t.Fatal("expected dial with wrong token to fail")
	}
	if badResp == nil || badResp.StatusCode != http.StatusUnauthorized {
		status := 0
		if badResp != nil {
			status = badResp.StatusCode
		}
		t.Fatalf("wrong token /ws/listen status = %d, want 401", status)
	}

	// 3. Real Pion WebRTC client connecting via /ws/listen?k=<token>
	wsURL := fmt.Sprintf("ws://127.0.0.1:%d/ws/listen?k=%s", srv.Port(), hostState.JoinToken)
	wsConn, _, err := websocket.DefaultDialer.Dial(wsURL, nil)
	if err != nil {
		t.Fatalf("dial listener ws failed: %v", err)
	}
	defer wsConn.Close()

	var wsWriteMu sync.Mutex
	sendWS := func(v any) error {
		wsWriteMu.Lock()
		defer wsWriteMu.Unlock()
		return wsConn.WriteJSON(v)
	}

	type pktInfo struct {
		ts  uint32
		len int
	}

	me := &webrtc.MediaEngine{}
	if err := me.RegisterDefaultCodecs(); err != nil {
		t.Fatalf("client RegisterDefaultCodecs failed: %v", err)
	}
	se := webrtc.SettingEngine{}
	se.SetIncludeLoopbackCandidate(true)
	api := webrtc.NewAPI(webrtc.WithMediaEngine(me), webrtc.WithSettingEngine(se))

	var pcMu sync.Mutex
	var curPC *webrtc.PeerConnection

	newClientPC := func() (*webrtc.PeerConnection, chan pktInfo) {
		p, err := api.NewPeerConnection(webrtc.Configuration{ICEServers: nil})
		if err != nil {
			t.Fatalf("client NewPeerConnection failed: %v", err)
		}
		if _, err := p.AddTransceiverFromKind(webrtc.RTPCodecTypeAudio, webrtc.RTPTransceiverInit{
			Direction: webrtc.RTPTransceiverDirectionRecvonly,
		}); err != nil {
			t.Fatalf("AddTransceiver failed: %v", err)
		}
		p.OnICECandidate(func(c *webrtc.ICECandidate) {
			if c != nil {
				_ = sendWS(map[string]any{
					"type":      "ice",
					"candidate": c.ToJSON(),
				})
			}
		})
		ch := make(chan pktInfo, 100)
		p.OnTrack(func(track *webrtc.TrackRemote, receiver *webrtc.RTPReceiver) {
			for {
				pkt, _, err := track.ReadRTP()
				if err != nil {
					return
				}
				select {
				case ch <- pktInfo{ts: pkt.Timestamp, len: len(pkt.Payload)}:
				default:
				}
			}
		})
		pcMu.Lock()
		curPC = p
		pcMu.Unlock()
		return p, ch
	}

	pc, pktCh := newClientPC()
	defer func() {
		pcMu.Lock()
		p := curPC
		pcMu.Unlock()
		if p != nil {
			_ = p.Close()
		}
	}()

	pongCh := make(chan map[string]any, 4)
	answerCh := make(chan string, 4)
	go func() {
		for {
			_, raw, err := wsConn.ReadMessage()
			if err != nil {
				return
			}
			var m map[string]any
			if err := json.Unmarshal(raw, &m); err != nil {
				continue
			}
			pcMu.Lock()
			activePC := curPC
			pcMu.Unlock()
			switch m["type"] {
			case "clock_pong":
				select {
				case pongCh <- m:
				default:
				}
			case "answer":
				if sdpStr, ok := m["sdp"].(string); ok && activePC != nil {
					_ = activePC.SetRemoteDescription(webrtc.SessionDescription{
						Type: webrtc.SDPTypeAnswer,
						SDP:  sdpStr,
					})
					select {
					case answerCh <- sdpStr:
					default:
					}
				}
			case "ice":
				if rawCand, ok := m["candidate"]; ok && rawCand != nil && activePC != nil {
					b, _ := json.Marshal(rawCand)
					var cand webrtc.ICECandidateInit
					if json.Unmarshal(b, &cand) == nil {
						_ = activePC.AddICECandidate(cand)
					}
				}
			}
		}
	}()

	// 3a. Clock ping/pong check
	if err := sendWS(map[string]any{"type": "clock_ping", "t1": 123.45}); err != nil {
		t.Fatalf("send clock_ping failed: %v", err)
	}
	select {
	case pong := <-pongCh:
		if pong["t1"] != 123.45 || pong["t2"].(float64) <= 0 || pong["t3"].(float64) < pong["t2"].(float64) {
			t.Fatalf("invalid clock_pong payload: %+v", pong)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for clock_pong")
	}

	// 3b. Offer / Answer negotiation
	offer, err := pc.CreateOffer(nil)
	if err != nil {
		t.Fatalf("client CreateOffer failed: %v", err)
	}
	if err := pc.SetLocalDescription(offer); err != nil {
		t.Fatalf("client SetLocalDescription failed: %v", err)
	}
	if err := sendWS(map[string]any{"type": "offer", "sdp": offer.SDP, "sdpType": offer.Type.String()}); err != nil {
		t.Fatalf("send offer failed: %v", err)
	}

	select {
	case ansSDP := <-answerCh:
		if !strings.Contains(ansSDP, "stereo=1") {
			t.Fatalf("expected answer SDP to contain stereo=1, got:\n%s", ansSDP)
		}
	case <-time.After(4 * time.Second):
		t.Fatal("timed out waiting for SDP answer")
	}

	// 3c. Receive >= 25 Opus RTP packets and verify 960-sample timestamp step
	var received []pktInfo
	deadline := time.After(6 * time.Second)
	for len(received) < 25 {
		select {
		case p := <-pktCh:
			if p.len <= 0 {
				t.Fatal("received empty Opus RTP payload")
			}
			received = append(received, p)
		case <-deadline:
			t.Fatalf("timed out receiving 25 Opus RTP packets, got %d", len(received))
		}
	}

	consecutiveStep960 := 0
	for i := 1; i < len(received); i++ {
		if received[i].ts-received[i-1].ts == 960 {
			consecutiveStep960++
		}
	}
	if consecutiveStep960 < 20 {
		t.Fatalf("expected >= 20 packet transitions with 960-sample step, got %d", consecutiveStep960)
	}

	// 3d. Report telemetry & prefs (left channel + -80ms manual offset + iOS gain)
	canSetFalse := false
	for i := 0; i < 2; i++ {
		if err := sendWS(map[string]any{
			"type":               "prefs",
			"channelMode":        "left",
			"userOffsetMs":       -80.0,
			"clientGain":         0.7,
			"clientCanSetVolume": canSetFalse,
			"name":               "Test iPhone",
		}); err != nil {
			t.Fatalf("send prefs failed: %v", err)
		}
		if err := sendWS(map[string]any{
			"type":            "telemetry",
			"wsRttMs":         6.0,
			"jitterBufferMs":  40.0,
			"audioOutputMs":   15.0,
			"packetsLost":     0,
			"packetsReceived": 25,
			"playing":         true,
		}); err != nil {
			t.Fatalf("send telemetry failed: %v", err)
		}
		time.Sleep(30 * time.Millisecond)
	}

	// Wait briefly for state propagation and inspect HostState
	var matched bool
	for retry := 0; retry < 20; retry++ {
		st := srv.HostState()
		if len(st.Sync.Listeners) == 1 {
			l := st.Sync.Listeners[0]
			if l.Name == "Test iPhone" &&
				l.Telemetry.ChannelMode == syncctrl.ChannelLeft &&
				l.Telemetry.UserOffsetMs == -80.0 &&
				l.SendDelayMs < st.Sync.ActiveTargetMs-70.0 {
				matched = true
				break
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	if !matched {
		st := srv.HostState()
		t.Fatalf("listener sync/prefs did not update as expected: %+v", st.Sync.Listeners)
	}

	// 4. Regression check: Pause ("leave") and Rejoin ("offer") on the same WebSocket
	_ = sendWS(map[string]any{"type": "leave"})
	_ = pc.Close()

	// Brief pause to confirm host removes the peer without tearing down the WebSocket
	time.Sleep(80 * time.Millisecond)

	pc2, pktCh2 := newClientPC()
	offer2, err := pc2.CreateOffer(nil)
	if err != nil {
		t.Fatalf("second CreateOffer failed: %v", err)
	}
	if err := pc2.SetLocalDescription(offer2); err != nil {
		t.Fatalf("second SetLocalDescription failed: %v", err)
	}
	if err := sendWS(map[string]any{"type": "offer", "sdp": offer2.SDP, "sdpType": offer2.Type.String()}); err != nil {
		t.Fatalf("send second offer failed: %v", err)
	}

	select {
	case ansSDP2 := <-answerCh:
		if !strings.Contains(ansSDP2, "stereo=1") {
			t.Fatalf("expected second answer SDP to contain stereo=1, got:\n%s", ansSDP2)
		}
	case <-time.After(4 * time.Second):
		t.Fatal("timed out waiting for second SDP answer after rejoin")
	}

	rejoinCount := 0
	rejoinDeadline := time.After(6 * time.Second)
	for rejoinCount < 10 {
		select {
		case p := <-pktCh2:
			if p.len <= 0 {
				t.Fatal("received empty Opus RTP payload after rejoin")
			}
			rejoinCount++
		case <-rejoinDeadline:
			t.Fatalf("timed out receiving 10 Opus RTP packets after rejoin, got %d", rejoinCount)
		}
	}
}
