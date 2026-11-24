(() => {
  const $ = (id) => document.getElementById(id);

  const remoteAudio = $('remoteAudio');
  const joinBtn = $('joinBtn');
  const statusPill = $('statusPill');
  const statusText = $('statusText');
  const errorBanner = $('errorBanner');
  const tokenCard = $('tokenCard');
  const tokenInput = $('tokenInput');
  const tokenSaveBtn = $('tokenSaveBtn');

  const volumeSlider = $('volumeSlider');
  const volumeVal = $('volumeVal');
  const volModeTag = $('volModeTag');
  const channelSelect = $('channelSelect');
  const offsetSlider = $('offsetSlider');
  const offsetVal = $('offsetVal');
  const offsetResetBtn = $('offsetResetBtn');
  const deviceNameInput = $('deviceNameInput');
  const saveNameBtn = $('saveNameBtn');

  const targetVal = $('targetVal');
  const rttVal = $('rttVal');
  const sendDelayVal = $('sendDelayVal');
  const jbVal = $('jbVal');
  const outLatVal = $('outLatVal');
  const basePathVal = $('basePathVal');
  const lossVal = $('lossVal');
  const connDetail = $('connDetail');

  const STORAGE_KEY_OFFSET = 'roomchorus.offsetMs';
  const STORAGE_KEY_CHANNEL = 'roomchorus.channelMode';
  const STORAGE_KEY_VOLUME = 'roomchorus.volume';
  const STORAGE_KEY_NAME = 'roomchorus.deviceName';
  const STORAGE_KEY_TOKEN = 'roomchorus.token';

  function safeGet(key) {
    try {
      return window.localStorage ? window.localStorage.getItem(key) : null;
    } catch (e) {
      return null;
    }
  }

  function safeSet(key, val) {
    try {
      if (window.localStorage) window.localStorage.setItem(key, val);
    } catch (e) {
      // Ignore storage quota or private browsing restrictions
    }
  }

  function errMsg(err) {
    if (!err) return '未知错误';
    if (err.message) return err.message;
    return String(err);
  }

  function defaultDeviceName() {
    const ua = navigator.userAgent || '';
    if (/iPhone/i.test(ua)) return 'iPhone';
    if (/iPad/i.test(ua)) return 'iPad';
    if (navigator.platform === 'MacIntel' && navigator.maxTouchPoints > 1) return 'iPad';
    if (/Android/i.test(ua)) return 'Android';
    if (/Macintosh/i.test(ua)) return 'Mac 浏览器';
    if (/Windows/i.test(ua)) return 'Windows 浏览器';
    return '局域网设备';
  }

  function readTokenFromLocation() {
    try {
      const params = new URLSearchParams(window.location.search);
      const k = (params.get('k') || '').trim();
      if (k) {
        safeSet(STORAGE_KEY_TOKEN, k);
        return k;
      }
    } catch (e) {
      // Fallback for older browsers
    }
    return (safeGet(STORAGE_KEY_TOKEN) || '').trim();
  }

  let joinToken = readTokenFromLocation();
  let ws = null;
  let pc = null;
  let joined = false;
  let canSetAudioVolume = true;
  let probedOutputLatencyMs = 0;
  let latestWsRttMs = 0;
  let latestClockOffsetMs = 0;
  let clockSamples = [];
  let prevJbDelay = 0;
  let prevJbEmitted = 0;
  let telemetryTimer = null;
  let clockTimer = null;
  let reconnectTimer = null;
  let connectWatchdog = null;
  let prefsThrottleTimer = null;
  let lastPacketsLost = 0;
  let lastPacketsRecv = 0;

  // Restore saved prefs
  const savedVol = safeGet(STORAGE_KEY_VOLUME);
  if (savedVol !== null) volumeSlider.value = savedVol;
  const savedOffset = safeGet(STORAGE_KEY_OFFSET);
  if (savedOffset !== null) offsetSlider.value = savedOffset;
  const savedChannel = safeGet(STORAGE_KEY_CHANNEL);
  if (savedChannel) channelSelect.value = savedChannel;
  deviceNameInput.value = safeGet(STORAGE_KEY_NAME) || defaultDeviceName();

  function refreshPrefLabels() {
    volumeVal.textContent = `${volumeSlider.value}%`;
    const off = Number(offsetSlider.value);
    offsetVal.textContent = `${off > 0 ? '+' : ''}${off} ms`;
  }
  refreshPrefLabels();

  function showError(msg) {
    if (!msg) {
      errorBanner.classList.add('hidden');
      errorBanner.textContent = '';
      return;
    }
    errorBanner.textContent = msg;
    errorBanner.classList.remove('hidden');
  }

  function isIOSLike() {
    const ua = navigator.userAgent || '';
    return /iPhone|iPad|iPod/i.test(ua) || (navigator.platform === 'MacIntel' && navigator.maxTouchPoints > 1);
  }

  function detectVolumeCapability() {
    if (isIOSLike()) {
      canSetAudioVolume = false;
      volModeTag.textContent = '(主机控音)';
      return;
    }
    try {
      const prev = remoteAudio.volume;
      const probe = prev === 0.5 ? 0.6 : 0.5;
      remoteAudio.volume = probe;
      canSetAudioVolume = Math.abs(remoteAudio.volume - probe) < 0.02;
      if (canSetAudioVolume) {
        remoteAudio.volume = Number(volumeSlider.value) / 100;
        volModeTag.textContent = '(本地音量)';
      } else {
        volModeTag.textContent = '(主机控音)';
      }
    } catch (e) {
      canSetAudioVolume = false;
      volModeTag.textContent = '(主机控音)';
    }
  }

  function probeOutputLatencyOnce() {
    try {
      const AC = window.AudioContext || window.webkitAudioContext;
      if (!AC) {
        if (isIOSLike()) probedOutputLatencyMs = 28;
        outLatVal.textContent = `${probedOutputLatencyMs.toFixed(1)} ms`;
        return;
      }
      const ctx = new AC();
      const outLat = Number(ctx.outputLatency || 0);
      const baseLat = Number(ctx.baseLatency || 0);
      if (Number.isFinite(outLat) && outLat > 0.005 && outLat < 1.0) {
        probedOutputLatencyMs = outLat * 1000;
      } else if (isIOSLike()) {
        probedOutputLatencyMs = Math.max(baseLat * 1000 + 25, 28);
      } else if (Number.isFinite(baseLat) && baseLat > 0 && baseLat < 1.0) {
        probedOutputLatencyMs = baseLat * 1000;
      }
      Promise.race([
        ctx.close().catch(() => {}),
        new Promise((resolve) => setTimeout(resolve, 250)),
      ]);
    } catch (e) {
      if (isIOSLike()) probedOutputLatencyMs = 28;
    }
    outLatVal.textContent = `${probedOutputLatencyMs.toFixed(1)} ms`;
  }

  function sendWS(obj) {
    if (ws && ws.readyState === WebSocket.OPEN) {
      ws.send(JSON.stringify(obj));
    }
  }

  function sendPrefsNow() {
    sendWS({
      type: 'prefs',
      channelMode: channelSelect.value,
      userOffsetMs: Number(offsetSlider.value),
      clientGain: Number(volumeSlider.value) / 100,
      clientCanSetVolume: canSetAudioVolume,
      name: deviceNameInput.value.trim() || defaultDeviceName(),
    });
  }

  function schedulePrefsSend() {
    if (prefsThrottleTimer) return;
    prefsThrottleTimer = setTimeout(() => {
      prefsThrottleTimer = null;
      sendPrefsNow();
    }, 80);
  }

  function runClockBurst() {
    clockSamples = [];
    for (let i = 0; i < 4; i++) {
      setTimeout(() => {
        sendWS({ type: 'clock_ping', t1: performance.now() });
      }, i * 60);
    }
  }

  function handleClockPong(msg) {
    const t4 = performance.now();
    const t1 = Number(msg.t1);
    const t2 = Number(msg.t2);
    const t3 = Number(msg.t3);
    if (![t1, t2, t3].every(Number.isFinite)) return;
    const rtt = Math.max(0, (t4 - t1) - (t3 - t2));
    const offset = ((t2 - t1) + (t3 - t4)) / 2;
    clockSamples.push({ rtt, offset });
    if (clockSamples.length > 6) clockSamples.shift();
    let best = clockSamples[0];
    for (const s of clockSamples) {
      if (s.rtt < best.rtt) best = s;
    }
    latestWsRttMs = best.rtt;
    latestClockOffsetMs = best.offset;
    rttVal.textContent = `${best.rtt.toFixed(1)} ms`;
  }

  function injectStereoOpusSDP(sdp) {
    const m = sdp.match(/^a=rtpmap:(\d+)\s+opus\/48000\/2/m);
    if (!m) return sdp;
    const pt = m[1];
    const lineSep = sdp.indexOf('\r\n') >= 0 ? '\r\n' : '\n';
    const lines = sdp.split(/\r?\n/);
    const prefix = `a=fmtp:${pt} `;
    for (let i = 0; i < lines.length; i++) {
      if (lines[i].indexOf(prefix) === 0) {
        const parts = lines[i].slice(prefix.length).split(';').map((s) => s.trim()).filter(Boolean);
        const kv = new Map();
        for (const p of parts) {
          const eq = p.indexOf('=');
          if (eq > 0) kv.set(p.slice(0, eq), p.slice(eq + 1));
        }
        kv.set('stereo', '1');
        kv.set('sprop-stereo', '1');
        kv.set('useinbandfec', '1');
        lines[i] = prefix + Array.from(kv.entries()).map(([k, v]) => `${k}=${v}`).join(';');
        break;
      }
    }
    return lines.join(lineSep);
  }

  function setupMediaSession() {
    if (!('mediaSession' in navigator) || !window.MediaMetadata) return;
    try {
      navigator.mediaSession.metadata = new MediaMetadata({
        title: 'RoomChorus 局域网同步音频',
        artist: 'Mac 实时广播',
        album: 'LAN Sync',
      });
      navigator.mediaSession.setActionHandler('play', () => {
        if (!joined) joinPlayback();
        else remoteAudio.play().catch(() => {});
      });
      navigator.mediaSession.setActionHandler('pause', () => {
        leavePlayback();
      });
      navigator.mediaSession.playbackState = joined ? 'playing' : 'paused';
    } catch (e) {
      // MediaSession optional
    }
  }

  async function setupWebRTC() {
    if (!window.RTCPeerConnection) {
      throw new Error('当前浏览器限制或不支持 WebRTC（iPad/iPhone 请用系统 Safari 打开，并确认允许本地网络权限）');
    }
    closePeerOnly();
    prevJbDelay = 0;
    prevJbEmitted = 0;

    pc = new RTCPeerConnection({ iceServers: [] });
    pc.addTransceiver('audio', { direction: 'recvonly' });

    pc.onicecandidate = (e) => {
      if (e.candidate) {
        sendWS({ type: 'ice', candidate: e.candidate.toJSON() });
      }
    };

    pc.ontrack = (e) => {
      const stream = (e.streams && e.streams[0]) ? e.streams[0] : new MediaStream([e.track]);
      remoteAudio.srcObject = stream;
      if (canSetAudioVolume) {
        remoteAudio.volume = Number(volumeSlider.value) / 100;
      }
      if (joined) {
        remoteAudio.muted = false;
        remoteAudio.play().catch((err) => {
          showError(`浏览器阻止了自动播放，请再点一次上方按钮：${errMsg(err)}`);
        });
      } else {
        // Silently drain incoming WebRTC audio so Safari jitter buffer stays minimal before tap.
        remoteAudio.muted = true;
        remoteAudio.play().catch(() => {});
      }
    };

    pc.onconnectionstatechange = () => {
      if (!pc) return;
      connDetail.textContent = `WebRTC 状态: ${pc.connectionState}`;
      if (pc.connectionState === 'connected') {
        showError('');
        setupMediaSession();
        collectAndSendTelemetry();
      } else if (pc.connectionState === 'failed' || pc.connectionState === 'disconnected') {
        connDetail.textContent = joined ? 'WebRTC 连接中断，准备重连…' : `WebRTC 状态: ${pc.connectionState}`;
      }
    };

    const offer = await pc.createOffer();
    await pc.setLocalDescription(offer);
    sendWS({ type: 'offer', sdp: offer.sdp, sdpType: offer.type });
  }

  async function collectAndSendTelemetry() {
    if (!ws || ws.readyState !== WebSocket.OPEN) return;
    let jbMs = 45;
    let pktLost = lastPacketsLost;
    let pktRecv = lastPacketsRecv;
    const outLatMs = probedOutputLatencyMs;

    if (pc) {
      try {
        const stats = await pc.getStats();
        stats.forEach((report) => {
          if (report.type === 'inbound-rtp' && report.kind === 'audio') {
            pktLost = Number(report.packetsLost || 0);
            pktRecv = Number(report.packetsReceived || 0);
            if (typeof report.jitterBufferDelay === 'number' && typeof report.jitterBufferEmittedCount === 'number') {
              const dDelay = report.jitterBufferDelay - prevJbDelay;
              const dEmit = report.jitterBufferEmittedCount - prevJbEmitted;
              if (dDelay >= 0 && dEmit > 0) {
                jbMs = (dDelay / dEmit) * 1000;
              } else if (report.jitterBufferEmittedCount > 0) {
                jbMs = (report.jitterBufferDelay / report.jitterBufferEmittedCount) * 1000;
              }
              prevJbDelay = report.jitterBufferDelay;
              prevJbEmitted = report.jitterBufferEmittedCount;
            }
          }
        });
      } catch (e) {
        // Ignore transient stats errors
      }
    }

    lastPacketsLost = pktLost;
    lastPacketsRecv = pktRecv;
    jbMs = Math.max(0, Math.min(1500, jbMs));
    jbVal.textContent = `${jbMs.toFixed(1)} ms`;
    lossVal.textContent = String(pktLost);

    sendWS({
      type: 'telemetry',
      wsRttMs: latestWsRttMs,
      jitterBufferMs: jbMs,
      audioOutputMs: outLatMs,
      packetsLost: pktLost,
      packetsReceived: pktRecv,
      clockOffsetMs: latestClockOffsetMs,
      playing: joined && !remoteAudio.paused && !remoteAudio.muted,
      background: document.hidden,
    });
  }

  function startBackgroundTimers() {
    if (!telemetryTimer) {
      telemetryTimer = setInterval(collectAndSendTelemetry, 1000);
    }
    if (!clockTimer) {
      clockTimer = setInterval(runClockBurst, 10000);
    }
  }

  function stopBackgroundTimers() {
    if (telemetryTimer) {
      clearInterval(telemetryTimer);
      telemetryTimer = null;
    }
    if (clockTimer) {
      clearInterval(clockTimer);
      clockTimer = null;
    }
  }

  function clearConnectWatchdog() {
    if (connectWatchdog) {
      clearTimeout(connectWatchdog);
      connectWatchdog = null;
    }
  }

  function connectWS() {
    if (ws && (ws.readyState === WebSocket.OPEN || ws.readyState === WebSocket.CONNECTING)) {
      return;
    }
    statusPill.classList.remove('live');
    statusText.textContent = '连接中…';

    const proto = window.location.protocol === 'https:' ? 'wss:' : 'ws:';
    const q = joinToken ? `?k=${encodeURIComponent(joinToken)}` : '';
    const wsUrl = `${proto}//${window.location.host}/ws/listen${q}`;

    clearConnectWatchdog();
    connectWatchdog = setTimeout(() => {
      if (!ws || ws.readyState !== WebSocket.OPEN) {
        statusText.textContent = '连接超时';
        showError('WebSocket 长时间未连上。请检查：1) iPad 是否开启 VPN/代理拦截局域网流量；2) 设置 → 隐私与安全性 → 本地网络 是否允许 Safari；3) 请用系统 Safari 打开而非 App 内置浏览器。');
      }
    }, 5000);

    try {
      ws = new WebSocket(wsUrl);
    } catch (e) {
      clearConnectWatchdog();
      statusText.textContent = '连接失败';
      showError(`无法创建 WebSocket 连接: ${errMsg(e)}`);
      return;
    }

    ws.onopen = async () => {
      clearConnectWatchdog();
      showError('');
      tokenCard.classList.add('hidden');
      runClockBurst();
      sendPrefsNow();
      startBackgroundTimers();
      try {
        await setupWebRTC();
      } catch (err) {
        showError(errMsg(err));
      }
    };

    ws.onerror = () => {
      connDetail.textContent = 'WebSocket 握手失败或连接异常';
      if (!ws || ws.readyState !== WebSocket.OPEN) {
        showError('无法建立实时连接（WebSocket 错误），请检查口令、本地网络权限或关闭代理后重试。');
      }
    };

    ws.onmessage = async (evt) => {
      let msg;
      try {
        msg = JSON.parse(evt.data);
      } catch (e) {
        return;
      }

      if (msg.type === 'clock_pong') {
        handleClockPong(msg);
      } else if (msg.type === 'answer' && pc) {
        try {
          const answerSdp = injectStereoOpusSDP(msg.sdp);
          await pc.setRemoteDescription(new RTCSessionDescription({ type: msg.sdpType || 'answer', sdp: answerSdp }));
        } catch (err) {
          showError(`SDP 协商失败: ${errMsg(err)}`);
        }
      } else if (msg.type === 'ice' && pc && msg.candidate) {
        try {
          await pc.addICECandidate(msg.candidate);
        } catch (e) {
          // Ignore late candidate
        }
      } else if (msg.type === 'sync_state') {
        const broadcasting = Boolean(msg.broadcasting);
        statusPill.classList.toggle('live', broadcasting);
        statusText.textContent = broadcasting ? `正在广播 · ${msg.listenerCount || 0} 台在线` : '等待主机开播';

        if (typeof msg.displayTargetMs === 'number') {
          targetVal.textContent = `${Math.round(msg.displayTargetMs)} ms`;
        }
        if (typeof msg.yourSendDelayMs === 'number') {
          sendDelayVal.textContent = `${Math.round(msg.yourSendDelayMs)} ms`;
        }
        if (typeof msg.yourBasePathMs === 'number') {
          basePathVal.textContent = `${Math.round(msg.yourBasePathMs)} ms`;
        }
      } else if (msg.type === 'kicked') {
        showError('主机端已断开本设备连接');
        leavePlayback();
      }
    };

    ws.onclose = async () => {
      clearConnectWatchdog();
      statusPill.classList.remove('live');
      statusText.textContent = '连接已断开';
      stopBackgroundTimers();
      try {
        const checkQ = joinToken ? `?k=${encodeURIComponent(joinToken)}` : '';
        const res = await fetch(`/api/auth-check${checkQ}`, { cache: 'no-store' });
        if (res.status === 401) {
          tokenCard.classList.remove('hidden');
          showError('口令无效或已更换，请输入最新 8 位口令');
          leavePlayback();
          return;
        }
      } catch (e) {
        // Network temporarily unreachable; proceed to reconnect.
      }
      if (reconnectTimer) clearTimeout(reconnectTimer);
      reconnectTimer = setTimeout(() => {
        connectWS();
      }, 2000);
    };
  }

  function closePeerOnly() {
    if (pc) {
      try {
        pc.ontrack = null;
        pc.onicecandidate = null;
        pc.onconnectionstatechange = null;
        pc.close();
      } catch (e) {
        // Ignore
      }
      pc = null;
    }
    if (remoteAudio.srcObject) {
      remoteAudio.srcObject = null;
    }
  }

  function joinPlayback() {
    try {
      if (!window.RTCPeerConnection) {
        showError('当前浏览器不支持或禁用了 WebRTC，请使用系统 Safari 打开并允许本地网络访问');
        return;
      }
      joined = true;
      joinBtn.textContent = '正在同步收听 · 点击暂停';
      joinBtn.classList.remove('primary');
      joinBtn.classList.add('danger');

      detectVolumeCapability();
      probeOutputLatencyOnce();

      // Call play() synchronously inside user gesture if stream is already attached.
      remoteAudio.muted = false;
      if (remoteAudio.srcObject) {
        remoteAudio.play().catch((err) => {
          showError(`播放未启动，请再点一次上方按钮：${errMsg(err)}`);
        });
      }

      if (!ws || ws.readyState !== WebSocket.OPEN) {
        connectWS();
      } else {
        startBackgroundTimers();
        sendPrefsNow();
        if (!pc || pc.connectionState === 'failed' || pc.connectionState === 'closed' || pc.connectionState === 'disconnected') {
          setupWebRTC().catch((err) => showError(errMsg(err)));
        } else {
          collectAndSendTelemetry();
        }
      }
      setupMediaSession();
    } catch (err) {
      showError(errMsg(err));
    }
  }

  function leavePlayback() {
    joined = false;
    joinBtn.textContent = '加入同步收听';
    joinBtn.classList.add('primary');
    joinBtn.classList.remove('danger');

    if (reconnectTimer) {
      clearTimeout(reconnectTimer);
      reconnectTimer = null;
    }
    try {
      remoteAudio.pause();
    } catch (e) {
      // Ignore
    }
    closePeerOnly();
    sendWS({ type: 'leave' });
    sendPrefsNow();
    collectAndSendTelemetry();
    connDetail.textContent = '已暂停收听';
    setupMediaSession();
  }

  remoteAudio.addEventListener('playing', () => {
    collectAndSendTelemetry();
  });
  remoteAudio.addEventListener('pause', () => {
    collectAndSendTelemetry();
  });

  joinBtn.addEventListener('click', () => {
    if (joined) leavePlayback();
    else joinPlayback();
  });

  volumeSlider.addEventListener('input', () => {
    safeSet(STORAGE_KEY_VOLUME, volumeSlider.value);
    refreshPrefLabels();
    if (canSetAudioVolume) {
      remoteAudio.volume = Number(volumeSlider.value) / 100;
    } else {
      schedulePrefsSend();
    }
  });

  channelSelect.addEventListener('change', () => {
    safeSet(STORAGE_KEY_CHANNEL, channelSelect.value);
    sendPrefsNow();
  });

  offsetSlider.addEventListener('input', () => {
    safeSet(STORAGE_KEY_OFFSET, offsetSlider.value);
    refreshPrefLabels();
    schedulePrefsSend();
  });

  offsetResetBtn.addEventListener('click', () => {
    offsetSlider.value = '0';
    safeSet(STORAGE_KEY_OFFSET, '0');
    refreshPrefLabels();
    sendPrefsNow();
  });

  saveNameBtn.addEventListener('click', () => {
    const name = deviceNameInput.value.trim() || defaultDeviceName();
    deviceNameInput.value = name;
    safeSet(STORAGE_KEY_NAME, name);
    sendPrefsNow();
  });

  tokenSaveBtn.addEventListener('click', () => {
    joinToken = tokenInput.value.trim();
    safeSet(STORAGE_KEY_TOKEN, joinToken);
    if (ws) {
      try {
        ws.close();
      } catch (e) {
        // Ignore
      }
      ws = null;
    }
    connectWS();
  });

  document.addEventListener('visibilitychange', () => {
    if (!document.hidden && joined) {
      runClockBurst();
      collectAndSendTelemetry();
    }
  });

  if (!window.RTCPeerConnection) {
    showError('当前浏览器不支持或禁用了 WebRTC（iPad/iPhone 请使用系统 Safari 打开并允许本地网络权限）');
  }
  detectVolumeCapability();
  connectWS();
})();
