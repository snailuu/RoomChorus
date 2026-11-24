package server

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"
	"github.com/pion/webrtc/v4"

	"roomchorus/internal/audio"
	"roomchorus/internal/netutil"
	"roomchorus/internal/opus"
	"roomchorus/internal/rtc"
	"roomchorus/internal/server/web"
	syncctrl "roomchorus/internal/sync"
)

// Config configures the RoomChorus HTTP, WebSocket, and audio host server.
type Config struct {
	Port                  int
	InterfaceName         string
	DisableToken          bool
	FixedToken            string
	AllowRemoteHost       bool
	DisableMonitorDefault bool
}

// HostConfig holds mutable runtime broadcast and monitor settings.
type HostConfig struct {
	Broadcasting     bool             `json:"broadcasting"`
	SourceMode       audio.SourceMode `json:"sourceMode"`
	InputDeviceID    uint32           `json:"inputDeviceId"`
	MuteOriginal     bool             `json:"muteOriginal"`
	MonitorEnabled   bool             `json:"monitorEnabled"`
	MonitorDeviceID  uint32           `json:"monitorDeviceId"`
	MonitorGain      float64          `json:"monitorGain"`
	HostUserOffsetMs float64          `json:"hostUserOffsetMs"`
	HostVolume       float64          `json:"hostVolume"`
	HostMuted        bool             `json:"hostMuted"`
	Bitrate          int              `json:"bitrate"`
	SelectedIface    string           `json:"selectedIface"`
	QRMode           string           `json:"qrMode"`
}

// HostStatePayload is serialized to /api/host/state and /ws/host.
type HostStatePayload struct {
	Config        HostConfig             `json:"config"`
	JoinToken     string                 `json:"joinToken"`
	TokenRequired bool                   `json:"tokenRequired"`
	IPv4URL       string                 `json:"ipv4Url"`
	MDNSURL       string                 `json:"mdnsUrl"`
	Interfaces    []netutil.NetInterface `json:"interfaces"`
	Devices       []audio.DeviceInfo     `json:"devices"`
	RmsLevel      float32                `json:"rmsLevel"`
	SilentWarning bool                   `json:"silentWarning"`
	Monitor       audio.MonitorStatus    `json:"monitor"`
	Sync          syncctrl.Snapshot      `json:"sync"`
	PeerStats     map[string][2]uint64   `json:"peerStats"`
	LastError     string                 `json:"lastError"`
}

type wsClient struct {
	id      string
	conn    *websocket.Conn
	writeMu sync.Mutex
}

func (c *wsClient) writeJSON(v any) error {
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	_ = c.conn.SetWriteDeadline(time.Now().Add(2 * time.Second))
	return c.conn.WriteJSON(v)
}

func (c *wsClient) close() {
	c.writeMu.Lock()
	_ = c.conn.Close()
	c.writeMu.Unlock()
}

// Server coordinates audio capture, sync control, WebRTC peers, and the embedded Web UI.
type Server struct {
	cfg      Config
	engine   *audio.Engine
	syncCtrl *syncctrl.Controller
	hub      *rtc.Hub

	opMu    sync.Mutex // serializes start/stop/config hardware operations
	stateMu sync.RWMutex

	hostCfg       HostConfig
	joinToken     string
	tokenRequired bool
	selectedIface netutil.NetInterface
	allIfaces     []netutil.NetInterface
	mdnsHost      string
	lastError     string

	hostClients     map[*wsClient]struct{}
	listenerClients map[string]*wsClient

	ln         net.Listener
	httpServer *http.Server
	mdnsAdv    *netutil.MDNSAdvertiser
	actualPort int

	upgrader websocket.Upgrader
	stopCh   chan struct{}
	doneCh   chan struct{}
}

// New creates a RoomChorus Server ready to listen and serve.
func New(cfg Config) (*Server, error) {
	iface, allIfaces, _ := netutil.PickDefaultInterface(cfg.InterfaceName)

	engine := audio.NewEngine()
	syncCtrl := syncctrl.NewController(6.5)
	hub, err := rtc.NewHub(engine, syncCtrl, opus.DefaultBitrate)
	if err != nil {
		return nil, fmt.Errorf("init rtc hub: %w", err)
	}

	token := ""
	tokenReq := !cfg.DisableToken
	if tokenReq {
		if t := strings.TrimSpace(cfg.FixedToken); t != "" {
			token = t
		} else {
			token = generateToken(8)
		}
	}

	hcfg := HostConfig{
		SourceMode:     audio.SourceSystem,
		MuteOriginal:   true,
		MonitorEnabled: !cfg.DisableMonitorDefault,
		MonitorGain:    0.85,
		HostVolume:     1.0,
		Bitrate:        opus.DefaultBitrate,
		SelectedIface:  iface.Name,
		QRMode:         "ipv4",
	}

	s := &Server{
		cfg:             cfg,
		engine:          engine,
		syncCtrl:        syncCtrl,
		hub:             hub,
		hostCfg:         hcfg,
		joinToken:       token,
		tokenRequired:   tokenReq,
		selectedIface:   iface,
		allIfaces:       allIfaces,
		mdnsHost:        netutil.LocalHostname(),
		hostClients:     make(map[*wsClient]struct{}),
		listenerClients: make(map[string]*wsClient),
		stopCh:          make(chan struct{}),
		doneCh:          make(chan struct{}),
		upgrader: websocket.Upgrader{
			ReadBufferSize:  8192,
			WriteBufferSize: 8192,
			CheckOrigin: func(r *http.Request) bool {
				return true
			},
		},
	}

	mux := http.NewServeMux()
	s.registerRoutes(mux)

	s.httpServer = &http.Server{
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
	}

	return s, nil
}

// Start binds the TCP listener, starts Bonjour mDNS advertisement, and launches background sync ticks.
func (s *Server) Start() error {
	addr := fmt.Sprintf("0.0.0.0:%d", s.cfg.Port)
	ln, err := net.Listen("tcp4", addr)
	if err != nil {
		return err
	}
	s.ln = ln
	if tcpAddr, ok := ln.Addr().(*net.TCPAddr); ok {
		s.stateMu.Lock()
		s.actualPort = tcpAddr.Port
		s.stateMu.Unlock()
	}

	s.mdnsAdv = netutil.StartMDNSAdvertiser("RoomChorus", s.Port())
	go s.runTicker()
	go func() {
		if err := s.httpServer.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			s.stateMu.Lock()
			s.lastError = err.Error()
			s.stateMu.Unlock()
		}
	}()
	return nil
}

// StartBroadcast begins audio capture and (if enabled) synchronized local host monitor playback.
func (s *Server) StartBroadcast(sourceOverride string) error {
	s.opMu.Lock()
	defer s.opMu.Unlock()

	s.stateMu.Lock()
	if sourceOverride != "" {
		s.hostCfg.SourceMode = normalizeSource(sourceOverride)
	}
	cfg := s.hostCfg
	s.lastError = ""
	s.stateMu.Unlock()

	// Start HostMonitor before System Tap so CoreAudio registers our process HAL client cleanly.
	var monLat float64
	if cfg.MonitorEnabled {
		lat, err := s.engine.StartMonitor(cfg.MonitorDeviceID)
		if err != nil {
			s.stateMu.Lock()
			s.lastError = "本机监听启动失败: " + err.Error()
			s.stateMu.Unlock()
			return err
		}
		monLat = lat
		s.syncCtrl.SetHostMonitor(true, monLat, cfg.HostUserOffsetMs)
	} else {
		s.engine.StopMonitor()
		s.syncCtrl.SetHostMonitor(false, 0, cfg.HostUserOffsetMs)
	}

	_ = s.hub.SetHostAudioConfig(cfg.HostVolume, cfg.HostMuted, cfg.MonitorGain, false, cfg.Bitrate)
	s.hub.ClearAllQueues()

	if err := s.engine.StartCapture(cfg.SourceMode, cfg.InputDeviceID, cfg.MuteOriginal, cfg.MonitorDeviceID, s.hub.ProcessFrame); err != nil {
		s.engine.StopMonitor()
		s.syncCtrl.SetHostMonitor(false, 0, cfg.HostUserOffsetMs)
		s.stateMu.Lock()
		s.hostCfg.Broadcasting = false
		s.lastError = err.Error()
		s.stateMu.Unlock()
		return err
	}

	s.stateMu.Lock()
	s.hostCfg.Broadcasting = true
	s.stateMu.Unlock()
	return nil
}

// StopBroadcast stops capture and host monitor playback and resets peer delay queues.
func (s *Server) StopBroadcast() {
	s.opMu.Lock()
	defer s.opMu.Unlock()

	s.engine.StopCapture()
	s.engine.StopMonitor()
	s.syncCtrl.SetHostMonitor(false, 0, 0)
	s.hub.ClearAllQueues()

	s.stateMu.Lock()
	s.hostCfg.Broadcasting = false
	s.stateMu.Unlock()
}

// Close gracefully stops broadcast, disconnects all peers/sockets, stops mDNS, and shuts down HTTP.
func (s *Server) Close(ctx context.Context) error {
	select {
	case <-s.stopCh:
	default:
		close(s.stopCh)
	}
	<-s.doneCh

	s.StopBroadcast()
	s.hub.Close()

	s.stateMu.Lock()
	hClients := make([]*wsClient, 0, len(s.hostClients))
	for c := range s.hostClients {
		hClients = append(hClients, c)
	}
	lClients := make([]*wsClient, 0, len(s.listenerClients))
	for _, c := range s.listenerClients {
		lClients = append(lClients, c)
	}
	s.stateMu.Unlock()

	for _, c := range hClients {
		c.close()
	}
	for _, c := range lClients {
		c.close()
	}

	if s.mdnsAdv != nil {
		s.mdnsAdv.Stop()
	}
	if s.httpServer != nil {
		return s.httpServer.Shutdown(ctx)
	}
	return nil
}

// Port returns the actual bound TCP port.
func (s *Server) Port() int {
	s.stateMu.RLock()
	defer s.stateMu.RUnlock()
	if s.actualPort > 0 {
		return s.actualPort
	}
	return s.cfg.Port
}

// JoinURLs returns the current LAN IPv4 and Bonjour .local URLs (including join token if active).
func (s *Server) JoinURLs() (ipv4URL string, mdnsURL string) {
	s.stateMu.RLock()
	ip := s.selectedIface.IP
	host := s.mdnsHost
	tok := s.joinToken
	req := s.tokenRequired
	port := s.actualPort
	if port == 0 {
		port = s.cfg.Port
	}
	s.stateMu.RUnlock()

	suffix := ""
	if req && tok != "" {
		suffix = "?k=" + tok
	}
	return fmt.Sprintf("http://%s:%d/%s", ip, port, suffix),
		fmt.Sprintf("http://%s.local:%d/%s", host, port, suffix)
}

// HostState returns a snapshot of configuration, devices, network info, and sync telemetry.
func (s *Server) HostState() HostStatePayload {
	s.stateMu.RLock()
	cfg := s.hostCfg
	tok := s.joinToken
	tokReq := s.tokenRequired
	ifaces := append([]netutil.NetInterface(nil), s.allIfaces...)
	lastErr := s.lastError
	s.stateMu.RUnlock()

	ipv4, mdns := s.JoinURLs()
	return HostStatePayload{
		Config:        cfg,
		JoinToken:     tok,
		TokenRequired: tokReq,
		IPv4URL:       ipv4,
		MDNSURL:       mdns,
		Interfaces:    ifaces,
		Devices:       audio.ListDevices(),
		RmsLevel:      s.engine.RmsLevel(),
		SilentWarning: s.engine.SilentWarning(),
		Monitor:       s.engine.MonitorStatus(),
		Sync:          s.syncCtrl.Snapshot(),
		PeerStats:     s.hub.PeerStats(),
		LastError:     lastErr,
	}
}

func (s *Server) registerRoutes(mux *http.ServeMux) {
	mux.HandleFunc("/", s.handleIndex)
	mux.HandleFunc("/host", s.handleHostPage)
	mux.HandleFunc("/assets/", s.handleAssets)
	mux.HandleFunc("/api/auth-check", s.handleAuthCheck)
	mux.HandleFunc("/api/qr.png", s.handleQR)
	mux.HandleFunc("/api/host/state", s.handleHostState)
	mux.HandleFunc("/api/host/start", s.handleHostStart)
	mux.HandleFunc("/api/host/stop", s.handleHostStop)
	mux.HandleFunc("/api/host/config", s.handleHostConfig)
	mux.HandleFunc("/api/host/listener/kick", s.handleKickListener)
	mux.HandleFunc("/api/host/listener/prefs", s.handleHostListenerPrefs)
	mux.HandleFunc("/ws/host", s.handleHostWS)
	mux.HandleFunc("/ws/listen", s.handleListenWS)
}

func (s *Server) allowHostRequest(w http.ResponseWriter, r *http.Request) bool {
	if s.cfg.AllowRemoteHost || netutil.IsLoopbackRemote(r.RemoteAddr) {
		return true
	}
	http.Error(w, "host console is restricted to localhost", http.StatusForbidden)
	return false
}

func (s *Server) allowLANRequest(w http.ResponseWriter, r *http.Request) bool {
	s.stateMu.RLock()
	subnet := s.selectedIface.Subnet
	s.stateMu.RUnlock()
	if netutil.IsSameSubnetOrLoopback(r.RemoteAddr, subnet) {
		return true
	}
	http.Error(w, "listener access restricted to local subnet", http.StatusForbidden)
	return false
}

func (s *Server) validToken(k string) bool {
	s.stateMu.RLock()
	req := s.tokenRequired
	tok := s.joinToken
	s.stateMu.RUnlock()
	if !req || tok == "" {
		return true
	}
	return strings.TrimSpace(k) == tok
}

func (s *Server) handleIndex(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	if !s.allowLANRequest(w, r) {
		log.Printf("[http] blocked / from %s (ua=%q)", r.RemoteAddr, r.UserAgent())
		return
	}
	if !netutil.IsLoopbackRemote(r.RemoteAddr) {
		log.Printf("[http] GET %s from %s (ua=%q)", r.URL.RequestURI(), r.RemoteAddr, r.UserAgent())
	}
	serveEmbeddedFile(w, "index.html", "text/html; charset=utf-8")
}

func (s *Server) handleHostPage(w http.ResponseWriter, r *http.Request) {
	if !s.allowHostRequest(w, r) {
		return
	}
	serveEmbeddedFile(w, "host.html", "text/html; charset=utf-8")
}

func (s *Server) handleAssets(w http.ResponseWriter, r *http.Request) {
	if !s.allowLANRequest(w, r) {
		return
	}
	name := strings.TrimPrefix(r.URL.Path, "/assets/")
	switch name {
	case "app.css":
		serveEmbeddedFile(w, "app.css", "text/css; charset=utf-8")
	case "host.js":
		if !s.allowHostRequest(w, r) {
			return
		}
		serveEmbeddedFile(w, "host.js", "text/javascript; charset=utf-8")
	case "listener.js":
		serveEmbeddedFile(w, "listener.js", "text/javascript; charset=utf-8")
	default:
		http.NotFound(w, r)
	}
}

func (s *Server) handleAuthCheck(w http.ResponseWriter, r *http.Request) {
	if !s.allowLANRequest(w, r) {
		return
	}
	if !s.validToken(r.URL.Query().Get("k")) {
		http.Error(w, "invalid token", http.StatusUnauthorized)
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func (s *Server) handleQR(w http.ResponseWriter, r *http.Request) {
	if !s.allowLANRequest(w, r) {
		return
	}
	mode := r.URL.Query().Get("mode")
	ipv4URL, mdnsURL := s.JoinURLs()
	target := ipv4URL
	if mode == "mdns" {
		target = mdnsURL
	}
	png, err := netutil.GeneratePNG(target, 360)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "image/png")
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write(png)
}

func (s *Server) handleHostState(w http.ResponseWriter, r *http.Request) {
	if !s.allowHostRequest(w, r) {
		return
	}
	writeJSON(w, http.StatusOK, s.HostState())
}

func (s *Server) handleHostStart(w http.ResponseWriter, r *http.Request) {
	if !s.allowHostRequest(w, r) {
		return
	}
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var body struct {
		SourceMode string `json:"sourceMode"`
	}
	_ = json.NewDecoder(r.Body).Decode(&body)
	if err := s.StartBroadcast(body.SourceMode); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{
			"error": err.Error(),
		})
		return
	}
	writeJSON(w, http.StatusOK, s.HostState())
}

func (s *Server) handleHostStop(w http.ResponseWriter, r *http.Request) {
	if !s.allowHostRequest(w, r) {
		return
	}
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	s.StopBroadcast()
	writeJSON(w, http.StatusOK, s.HostState())
}

type configPatch struct {
	SourceMode       *string  `json:"sourceMode"`
	InputDeviceID    *uint32  `json:"inputDeviceId"`
	MuteOriginal     *bool    `json:"muteOriginal"`
	MonitorEnabled   *bool    `json:"monitorEnabled"`
	MonitorDeviceID  *uint32  `json:"monitorDeviceId"`
	MonitorGain      *float64 `json:"monitorGain"`
	HostUserOffsetMs *float64 `json:"hostUserOffsetMs"`
	HostVolume       *float64 `json:"hostVolume"`
	HostMuted        *bool    `json:"hostMuted"`
	Bitrate          *int     `json:"bitrate"`
	SelectedIface    *string  `json:"selectedIface"`
	QRMode           *string  `json:"qrMode"`
	RotateToken      bool     `json:"rotateToken"`
}

func (s *Server) handleHostConfig(w http.ResponseWriter, r *http.Request) {
	if !s.allowHostRequest(w, r) {
		return
	}
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var p configPatch
	if err := json.NewDecoder(r.Body).Decode(&p); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	s.opMu.Lock()
	defer s.opMu.Unlock()

	s.stateMu.Lock()
	prev := s.hostCfg
	if p.SourceMode != nil {
		s.hostCfg.SourceMode = normalizeSource(*p.SourceMode)
	}
	if p.InputDeviceID != nil {
		s.hostCfg.InputDeviceID = *p.InputDeviceID
	}
	if p.MuteOriginal != nil {
		s.hostCfg.MuteOriginal = *p.MuteOriginal
	}
	if p.MonitorEnabled != nil {
		s.hostCfg.MonitorEnabled = *p.MonitorEnabled
	}
	if p.MonitorDeviceID != nil {
		s.hostCfg.MonitorDeviceID = *p.MonitorDeviceID
	}
	if p.MonitorGain != nil {
		s.hostCfg.MonitorGain = clampFloat(*p.MonitorGain, 0, 1)
	}
	if p.HostUserOffsetMs != nil {
		s.hostCfg.HostUserOffsetMs = clampFloat(*p.HostUserOffsetMs, -500, 500)
	}
	if p.HostVolume != nil {
		s.hostCfg.HostVolume = clampFloat(*p.HostVolume, 0, 1)
	}
	if p.HostMuted != nil {
		s.hostCfg.HostMuted = *p.HostMuted
	}
	if p.Bitrate != nil && *p.Bitrate >= opus.MinBitrate && *p.Bitrate <= opus.MaxBitrate {
		s.hostCfg.Bitrate = *p.Bitrate
	}
	if p.QRMode != nil && (*p.QRMode == "ipv4" || *p.QRMode == "mdns") {
		s.hostCfg.QRMode = *p.QRMode
	}
	if p.SelectedIface != nil && *p.SelectedIface != "" {
		if chosen, all, err := netutil.PickDefaultInterface(*p.SelectedIface); err == nil {
			s.selectedIface = chosen
			s.allIfaces = all
			s.hostCfg.SelectedIface = chosen.Name
		}
	}
	if p.RotateToken && s.tokenRequired {
		s.joinToken = generateToken(8)
	}
	cur := s.hostCfg
	s.lastError = ""
	s.stateMu.Unlock()

	if err := s.hub.SetHostAudioConfig(cur.HostVolume, cur.HostMuted, cur.MonitorGain, false, cur.Bitrate); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}

	if cur.Broadcasting {
		monitorChanged := cur.MonitorEnabled != prev.MonitorEnabled || cur.MonitorDeviceID != prev.MonitorDeviceID
		captureChanged := cur.SourceMode != prev.SourceMode ||
			cur.InputDeviceID != prev.InputDeviceID ||
			cur.MuteOriginal != prev.MuteOriginal ||
			(cur.SourceMode == audio.SourceSystem && cur.MonitorDeviceID != prev.MonitorDeviceID)

		if monitorChanged {
			if cur.MonitorEnabled {
				lat, err := s.engine.StartMonitor(cur.MonitorDeviceID)
				if err != nil {
					s.stateMu.Lock()
					s.lastError = err.Error()
					s.stateMu.Unlock()
					writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
					return
				}
				s.syncCtrl.SetHostMonitor(true, lat, cur.HostUserOffsetMs)
			} else {
				s.engine.StopMonitor()
				s.syncCtrl.SetHostMonitor(false, 0, cur.HostUserOffsetMs)
			}
		} else {
			mon := s.engine.MonitorStatus()
			s.syncCtrl.SetHostMonitor(cur.MonitorEnabled, mon.OutputLatencyMs, cur.HostUserOffsetMs)
		}

		if captureChanged {
			s.hub.ClearAllQueues()
			if err := s.engine.StartCapture(cur.SourceMode, cur.InputDeviceID, cur.MuteOriginal, cur.MonitorDeviceID, s.hub.ProcessFrame); err != nil {
				s.stateMu.Lock()
				s.lastError = err.Error()
				s.stateMu.Unlock()
				writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
				return
			}
		}
	}

	writeJSON(w, http.StatusOK, s.HostState())
}

func (s *Server) handleKickListener(w http.ResponseWriter, r *http.Request) {
	if !s.allowHostRequest(w, r) {
		return
	}
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var body struct {
		ID string `json:"id"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.ID == "" {
		http.Error(w, "missing listener id", http.StatusBadRequest)
		return
	}

	s.stateMu.Lock()
	client := s.listenerClients[body.ID]
	delete(s.listenerClients, body.ID)
	s.stateMu.Unlock()

	if client != nil {
		_ = client.writeJSON(map[string]string{"type": "kicked"})
		client.close()
	}
	s.hub.RemovePeer(body.ID)
	s.syncCtrl.RemoveListener(body.ID)
	writeJSON(w, http.StatusOK, s.HostState())
}

func (s *Server) handleHostListenerPrefs(w http.ResponseWriter, r *http.Request) {
	if !s.allowHostRequest(w, r) {
		return
	}
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var body struct {
		ID           string  `json:"id"`
		ChannelMode  string  `json:"channelMode"`
		UserOffsetMs float64 `json:"userOffsetMs"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.ID == "" {
		http.Error(w, "invalid payload", http.StatusBadRequest)
		return
	}

	_, _, curGain, curCanSet, ok := s.syncCtrl.ListenerSendDelay(body.ID)
	if !ok {
		http.Error(w, "listener not found", http.StatusNotFound)
		return
	}
	s.syncCtrl.UpdatePrefs(body.ID, syncctrl.ValidChannelMode(body.ChannelMode), body.UserOffsetMs, curGain, &curCanSet, "")
	writeJSON(w, http.StatusOK, s.HostState())
}

func (s *Server) handleHostWS(w http.ResponseWriter, r *http.Request) {
	if !s.allowHostRequest(w, r) {
		return
	}
	conn, err := s.upgrader.Upgrade(w, r, nil)
	if err != nil {
		return
	}
	client := &wsClient{conn: conn}

	s.stateMu.Lock()
	s.hostClients[client] = struct{}{}
	s.stateMu.Unlock()

	_ = client.writeJSON(s.HostState())

	defer func() {
		s.stateMu.Lock()
		delete(s.hostClients, client)
		s.stateMu.Unlock()
		client.close()
	}()

	for {
		if _, _, err := conn.ReadMessage(); err != nil {
			return
		}
	}
}

type listenMessage struct {
	Type               string                   `json:"type"`
	T1                 float64                  `json:"t1"`
	SDP                string                   `json:"sdp"`
	SDPType            string                   `json:"sdpType"`
	Candidate          *webrtc.ICECandidateInit `json:"candidate"`
	WsRttMs            float64                  `json:"wsRttMs"`
	JitterBufferMs     float64                  `json:"jitterBufferMs"`
	AudioOutputMs      float64                  `json:"audioOutputMs"`
	PacketsLost        uint64                   `json:"packetsLost"`
	PacketsReceived    uint64                   `json:"packetsReceived"`
	ClockOffsetMs      float64                  `json:"clockOffsetMs"`
	Playing            bool                     `json:"playing"`
	Background         bool                     `json:"background"`
	ChannelMode        string                   `json:"channelMode"`
	UserOffsetMs       float64                  `json:"userOffsetMs"`
	ClientGain         float64                  `json:"clientGain"`
	ClientCanSetVolume *bool                    `json:"clientCanSetVolume"`
	Name               string                   `json:"name"`
}

func (s *Server) handleListenWS(w http.ResponseWriter, r *http.Request) {
	if !s.allowLANRequest(w, r) {
		log.Printf("[ws/listen] 403 subnet reject from %s (ua=%q)", r.RemoteAddr, r.UserAgent())
		return
	}
	if !s.validToken(r.URL.Query().Get("k")) {
		log.Printf("[ws/listen] 401 invalid token from %s (ua=%q)", r.RemoteAddr, r.UserAgent())
		http.Error(w, "invalid join token", http.StatusUnauthorized)
		return
	}

	conn, err := s.upgrader.Upgrade(w, r, nil)
	if err != nil {
		log.Printf("[ws/listen] upgrade error from %s: %v", r.RemoteAddr, err)
		return
	}

	listenerID := "l-" + generateToken(10)
	ua := r.UserAgent()
	log.Printf("[ws/listen] connected id=%s remote=%s ua=%q", listenerID, r.RemoteAddr, ua)
	client := &wsClient{id: listenerID, conn: conn}

	s.stateMu.RLock()
	broadcasting := s.hostCfg.Broadcasting
	s.stateMu.RUnlock()

	s.syncCtrl.RegisterListener(listenerID, guessNameFromUA(ua), ua)

	var peer *rtc.ListenerPeer

	s.stateMu.Lock()
	s.listenerClients[listenerID] = client
	listenerCount := len(s.listenerClients)
	s.stateMu.Unlock()

	// Send immediate initial sync state so UI shows current broadcast status right away.
	snap := s.syncCtrl.Snapshot()
	_ = client.writeJSON(map[string]any{
		"type":            "sync_state",
		"broadcasting":    broadcasting,
		"listenerCount":   listenerCount,
		"displayTargetMs": snap.DisplayTargetMs,
		"activeTargetMs":  snap.ActiveTargetMs,
	})

	defer func() {
		s.stateMu.Lock()
		delete(s.listenerClients, listenerID)
		s.stateMu.Unlock()
		s.hub.RemovePeer(listenerID)
		s.syncCtrl.RemoveListener(listenerID)
		client.close()
	}()

	for {
		_, data, err := conn.ReadMessage()
		if err != nil {
			return
		}
		var msg listenMessage
		if err := json.Unmarshal(data, &msg); err != nil {
			continue
		}

		switch msg.Type {
		case "clock_ping":
			t2 := audio.NowMs()
			_ = client.writeJSON(map[string]any{
				"type": "clock_pong",
				"t1":   msg.T1,
				"t2":   t2,
				"t3":   audio.NowMs(),
			})

		case "offer":
			if peer != nil {
				s.hub.RemovePeer(listenerID)
				peer = nil
			}
			s.stateMu.RLock()
			allowedIP := s.selectedIface.IP
			s.stateMu.RUnlock()

			newPeer, err := rtc.NewListenerPeer(rtc.PeerConfig{
				ID:        listenerID,
				AllowedIP: allowedIP,
				SyncCtrl:  s.syncCtrl,
				OnICECandidate: func(c webrtc.ICECandidateInit) {
					_ = client.writeJSON(map[string]any{
						"type":      "ice",
						"candidate": c,
					})
				},
			})
			if err != nil {
				_ = client.writeJSON(map[string]any{
					"type":  "error",
					"error": err.Error(),
				})
				continue
			}
			peer = newPeer
			s.hub.AddPeer(peer)

			sdpType := webrtc.SDPTypeOffer
			if strings.EqualFold(msg.SDPType, "pranswer") {
				sdpType = webrtc.SDPTypePranswer
			}
			ans, err := peer.HandleOffer(webrtc.SessionDescription{
				Type: sdpType,
				SDP:  msg.SDP,
			})
			if err != nil {
				log.Printf("[ws/listen] offer error id=%s: %v", listenerID, err)
				s.hub.RemovePeer(listenerID)
				peer = nil
				_ = client.writeJSON(map[string]any{
					"type":  "error",
					"error": err.Error(),
				})
				continue
			}
			_ = client.writeJSON(map[string]any{
				"type":    "answer",
				"sdp":     ans.SDP,
				"sdpType": ans.Type.String(),
			})

		case "leave":
			if peer != nil {
				s.hub.RemovePeer(listenerID)
				peer = nil
			}

		case "ice":
			if peer != nil && msg.Candidate != nil {
				_ = peer.AddICECandidate(*msg.Candidate)
			}

		case "telemetry":
			_, chMode, curGain, curCanSet, _ := s.syncCtrl.ListenerSendDelay(listenerID)
			var curUserOffset float64
			for _, l := range s.syncCtrl.Snapshot().Listeners {
				if l.ID == listenerID {
					curUserOffset = l.Telemetry.UserOffsetMs
					break
				}
			}
			s.syncCtrl.ReportTelemetry(listenerID, syncctrl.ListenerTelemetry{
				WsRttMs:            msg.WsRttMs,
				JitterBufferMs:     msg.JitterBufferMs,
				AudioOutputMs:      msg.AudioOutputMs,
				PacketsLost:        msg.PacketsLost,
				PacketsReceived:    msg.PacketsReceived,
				ClockOffsetMs:      msg.ClockOffsetMs,
				UserOffsetMs:       curUserOffset,
				ChannelMode:        chMode,
				ClientGain:         curGain,
				ClientCanSetVolume: curCanSet,
				Playing:            msg.Playing,
				Background:         msg.Background,
			}, time.Now())

		case "prefs":
			s.syncCtrl.UpdatePrefs(
				listenerID,
				syncctrl.ValidChannelMode(msg.ChannelMode),
				msg.UserOffsetMs,
				msg.ClientGain,
				msg.ClientCanSetVolume,
				strings.TrimSpace(msg.Name),
			)
		}
	}
}

func (s *Server) runTicker() {
	defer close(s.doneCh)
	ticker := time.NewTicker(250 * time.Millisecond)
	defer ticker.Stop()

	tickCount := 0
	for {
		select {
		case <-s.stopCh:
			return
		case now := <-ticker.C:
			tickCount++
			snap := s.syncCtrl.Tick(now)

			// Update HostMonitor params smoothly as ActiveTargetMs slews.
			s.stateMu.RLock()
			cfg := s.hostCfg
			hClients := make([]*wsClient, 0, len(s.hostClients))
			for c := range s.hostClients {
				hClients = append(hClients, c)
			}
			lClients := make(map[string]*wsClient, len(s.listenerClients))
			for id, c := range s.listenerClients {
				lClients[id] = c
			}
			s.stateMu.RUnlock()

			if cfg.Broadcasting && cfg.MonitorEnabled {
				s.engine.SetMonitorParams(snap.HostDelayMs, cfg.MonitorGain, false)
			}

			if len(hClients) > 0 {
				payload := s.HostState()
				for _, c := range hClients {
					if err := c.writeJSON(payload); err != nil {
						s.stateMu.Lock()
						delete(s.hostClients, c)
						s.stateMu.Unlock()
						c.close()
					}
				}
			}

			// Send listener sync summaries once per second.
			if tickCount%4 == 0 && len(lClients) > 0 {
				byID := make(map[string]syncctrl.ListenerSyncState, len(snap.Listeners))
				for _, l := range snap.Listeners {
					byID[l.ID] = l
				}
				count := len(lClients)
				for id, c := range lClients {
					ls := byID[id]
					_ = c.writeJSON(map[string]any{
						"type":            "sync_state",
						"broadcasting":    cfg.Broadcasting,
						"listenerCount":   count,
						"displayTargetMs": snap.DisplayTargetMs,
						"activeTargetMs":  snap.ActiveTargetMs,
						"yourSendDelayMs": ls.SendDelayMs,
						"yourBasePathMs":  ls.EffectiveBasePathMs,
					})
				}
			}
		}
	}
}

func serveEmbeddedFile(w http.ResponseWriter, name, contentType string) {
	data, err := fs.ReadFile(web.Assets, name)
	if err != nil {
		http.Error(w, "asset not found", http.StatusNotFound)
		return
	}
	w.Header().Set("Content-Type", contentType)
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Length", strconv.Itoa(len(data)))
	_, _ = w.Write(data)
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func normalizeSource(s string) audio.SourceMode {
	switch audio.SourceMode(s) {
	case audio.SourceInput:
		return audio.SourceInput
	case audio.SourceTestTone:
		return audio.SourceTestTone
	default:
		return audio.SourceSystem
	}
}

func generateToken(n int) string {
	b := make([]byte, (n+1)/2)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)[:n]
}

func guessNameFromUA(ua string) string {
	switch {
	case strings.Contains(ua, "iPhone"):
		return "iPhone"
	case strings.Contains(ua, "iPad"):
		return "iPad"
	case strings.Contains(ua, "Android"):
		return "Android"
	case strings.Contains(ua, "Macintosh"):
		return "Mac 浏览器"
	case strings.Contains(ua, "Windows"):
		return "Windows 浏览器"
	default:
		return "局域网听众"
	}
}

func clampFloat(v, lo, hi float64) float64 {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}
