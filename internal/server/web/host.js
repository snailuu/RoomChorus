(() => {
  const $ = (id) => document.getElementById(id);

  const broadcastPill = $('broadcastPill');
  const broadcastText = $('broadcastText');
  const toggleBroadcastBtn = $('toggleBroadcastBtn');
  const silentWarnBanner = $('silentWarnBanner');
  const switchTestToneBtn = $('switchTestToneBtn');
  const errorBanner = $('errorBanner');

  const sourceSegment = $('sourceSegment');
  const inputDeviceField = $('inputDeviceField');
  const inputDeviceSelect = $('inputDeviceSelect');
  const meterFill = $('meterFill');
  const rmsText = $('rmsText');

  const hostVolumeSlider = $('hostVolumeSlider');
  const hostVolumeVal = $('hostVolumeVal');
  const bitrateSelect = $('bitrateSelect');
  const muteOriginalCheck = $('muteOriginalCheck');
  const monitorEnableCheck = $('monitorEnableCheck');
  const monitorPanel = $('monitorPanel');
  const monitorDeviceSelect = $('monitorDeviceSelect');
  const monitorGainSlider = $('monitorGainSlider');
  const monitorGainVal = $('monitorGainVal');
  const hostOffsetSlider = $('hostOffsetSlider');
  const hostOffsetVal = $('hostOffsetVal');

  const listenerBadge = $('listenerBadge');
  const qrModeSegment = $('qrModeSegment');
  const qrImage = $('qrImage');
  const joinUrlBox = $('joinUrlBox');
  const copyUrlBtn = $('copyUrlBtn');
  const rotateTokenBtn = $('rotateTokenBtn');
  const tokenBadge = $('tokenBadge');
  const ifaceSelect = $('ifaceSelect');

  const displayTargetVal = $('displayTargetVal');
  const activeTargetVal = $('activeTargetVal');
  const hostLatVal = $('hostLatVal');
  const hostCompVal = $('hostCompVal');
  const listenersBody = $('listenersBody');
  const localListenerLink = $('localListenerLink');

  let state = null;
  let currentQrMode = 'ipv4';
  let currentJoinUrl = '';
  let configDebounce = null;
  let lastQrKey = '';

  async function postJSON(url, body = {}) {
    const res = await fetch(url, {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify(body),
    });
    const data = await res.json().catch(() => ({}));
    if (!res.ok) {
      throw new Error(data.error || `请求失败 (${res.status})`);
    }
    renderState(data);
    return data;
  }

  function showError(msg) {
    if (!msg) {
      errorBanner.classList.add('hidden');
      errorBanner.textContent = '';
      return;
    }
    errorBanner.textContent = msg;
    errorBanner.classList.remove('hidden');
  }

  function populateSelect(el, items, currentValue, mapper) {
    const key = JSON.stringify(items.map(mapper)) + '::' + String(currentValue);
    if (el.dataset.renderKey === key) return;
    el.dataset.renderKey = key;
    const frag = document.createDocumentFragment();
    for (const item of items) {
      const m = mapper(item);
      const opt = document.createElement('option');
      opt.value = String(m.value);
      opt.textContent = m.label;
      if (String(m.value) === String(currentValue)) opt.selected = true;
      frag.appendChild(opt);
    }
    el.replaceChildren(frag);
  }

  function renderState(s) {
    if (!s || !s.config) return;
    state = s;
    const cfg = s.config;

    broadcastPill.classList.toggle('live', cfg.broadcasting);
    broadcastText.textContent = cfg.broadcasting ? '广播中' : '待机';
    toggleBroadcastBtn.textContent = cfg.broadcasting ? '停止广播' : '开始广播';
    toggleBroadcastBtn.classList.toggle('primary', !cfg.broadcasting);
    toggleBroadcastBtn.classList.toggle('danger', cfg.broadcasting);

    silentWarnBanner.classList.toggle('hidden', !s.silentWarning);
    showError(s.lastError || '');

    const pct = Math.min(100, Math.round(Math.sqrt(Math.max(0, s.rmsLevel || 0)) * 100));
    meterFill.style.width = `${pct}%`;
    rmsText.textContent = `电平 ${pct}%`;

    sourceSegment.querySelectorAll('button').forEach((btn) => {
      btn.classList.toggle('active', btn.dataset.source === cfg.sourceMode);
    });
    inputDeviceField.classList.toggle('hidden', cfg.sourceMode !== 'input');

    const inDevices = (s.devices || []).filter((d) => d.hasInput);
    populateSelect(inputDeviceSelect, [{ id: 0, name: '系统默认输入' }, ...inDevices], cfg.inputDeviceId || 0, (d) => ({
      value: d.id,
      label: d.isDefaultInput && d.id !== 0 ? `${d.name} (默认)` : d.name,
    }));

    const outDevices = (s.devices || []).filter((d) => d.hasOutput);
    populateSelect(monitorDeviceSelect, [{ id: 0, name: '系统默认扬声器' }, ...outDevices], cfg.monitorDeviceId || 0, (d) => ({
      value: d.id,
      label: d.isDefaultOutput && d.id !== 0 ? `${d.name} (默认)` : d.name,
    }));

    populateSelect(ifaceSelect, s.interfaces || [], cfg.selectedIface || '', (i) => ({
      value: i.name,
      label: `${i.name} · ${i.ip}`,
    }));

    if (document.activeElement !== hostVolumeSlider) {
      hostVolumeSlider.value = Math.round((cfg.hostVolume ?? 1) * 100);
    }
    hostVolumeVal.textContent = `${hostVolumeSlider.value}%`;

    if (document.activeElement !== bitrateSelect) {
      bitrateSelect.value = String(cfg.bitrate || 192000);
    }

    muteOriginalCheck.checked = Boolean(cfg.muteOriginal);
    monitorEnableCheck.checked = Boolean(cfg.monitorEnabled);
    monitorPanel.classList.toggle('hidden', !cfg.monitorEnabled);

    if (document.activeElement !== monitorGainSlider) {
      monitorGainSlider.value = Math.round((cfg.monitorGain ?? 0.85) * 100);
    }
    monitorGainVal.textContent = `${monitorGainSlider.value}%`;

    if (document.activeElement !== hostOffsetSlider) {
      hostOffsetSlider.value = Math.round(cfg.hostUserOffsetMs || 0);
    }
    const off = Number(hostOffsetSlider.value);
    hostOffsetVal.textContent = `${off > 0 ? '+' : ''}${off} ms`;

    currentQrMode = cfg.qrMode || currentQrMode;
    qrModeSegment.querySelectorAll('button').forEach((btn) => {
      btn.classList.toggle('active', btn.dataset.mode === currentQrMode);
    });

    currentJoinUrl = currentQrMode === 'mdns' ? s.mdnsUrl : s.ipv4Url;
    joinUrlBox.textContent = currentJoinUrl || '—';
    tokenBadge.textContent = s.tokenRequired ? (s.joinToken || '—') : '免密';
    if (localListenerLink) {
      localListenerLink.href = s.tokenRequired && s.joinToken ? `/?k=${encodeURIComponent(s.joinToken)}` : '/';
    }

    const qrKey = `${currentQrMode}:${currentJoinUrl}`;
    if (qrKey !== lastQrKey) {
      lastQrKey = qrKey;
      qrImage.src = `/api/qr.png?mode=${encodeURIComponent(currentQrMode)}&t=${Date.now()}`;
    }

    const syncSnap = s.sync || {};
    const listeners = syncSnap.listeners || [];
    listenerBadge.textContent = `${listeners.length} 台在线`;

    displayTargetVal.textContent = `${Math.round(syncSnap.displayTargetMs || 300)} ms`;
    activeTargetVal.textContent = `${Math.round(syncSnap.activeTargetMs || 300)} ms`;
    hostLatVal.textContent = s.monitor?.enabled ? `${(s.monitor.outputLatencyMs || 0).toFixed(1)} ms` : '关闭';
    hostCompVal.textContent = s.monitor?.enabled ? `${Math.round(syncSnap.hostDelayMs || 0)} ms` : '—';

    renderListenersTable(listeners);
  }

  function renderListenersTable(listeners) {
    if (!listeners.length) {
      listenersBody.innerHTML = '<tr><td colspan="9" class="muted">暂无收听设备连接，请用手机或平板扫描上方二维码。</td></tr>';
      return;
    }

    const activeEl = document.activeElement;
    if (activeEl && listenersBody.contains(activeEl)) {
      return; // Avoid interrupting dropdown/input interaction while editing row
    }

    listenersBody.innerHTML = '';
    for (const l of listeners) {
      const tel = l.telemetry || {};
      const tr = document.createElement('tr');

      const nameTd = document.createElement('td');
      nameTd.innerHTML = `<strong>${escapeHtml(l.name || l.id)}</strong><div class="muted">${tel.playing ? (tel.background ? '后台收听' : '播放中') : '已连接 · 等待点击播放'}</div>`;

      const rttTd = document.createElement('td');
      rttTd.textContent = `${(tel.wsRttMs || 0).toFixed(1)} ms`;

      const jbTd = document.createElement('td');
      jbTd.textContent = `${(tel.jitterBufferMs || 0).toFixed(0)} + ${(tel.audioOutputMs || 0).toFixed(0)} ms`;

      const baseTd = document.createElement('td');
      baseTd.textContent = `${Math.round(l.effectiveBasePathMs || 0)} ms`;

      const compTd = document.createElement('td');
      compTd.innerHTML = `<strong>${Math.round(l.sendDelayMs || 0)} ms</strong>`;

      const lossTd = document.createElement('td');
      lossTd.textContent = String(tel.packetsLost || 0);

      const chTd = document.createElement('td');
      const sel = document.createElement('select');
      sel.style.minWidth = '88px';
      for (const [val, label] of [['stereo', '立体声'], ['left', '左声道'], ['right', '右声道'], ['mono', '单声道']]) {
        const opt = document.createElement('option');
        opt.value = val;
        opt.textContent = label;
        if ((tel.channelMode || 'stereo') === val) opt.selected = true;
        sel.appendChild(opt);
      }
      sel.onchange = () => updateListenerPrefs(l.id, { channelMode: sel.value, userOffsetMs: tel.userOffsetMs || 0 });
      chTd.appendChild(sel);

      const offTd = document.createElement('td');
      const offWrap = document.createElement('div');
      offWrap.className = 'row-actions';
      const minusBtn = document.createElement('button');
      minusBtn.className = 'sm';
      minusBtn.textContent = '-10';
      minusBtn.onclick = () => updateListenerPrefs(l.id, { channelMode: tel.channelMode || 'stereo', userOffsetMs: (tel.userOffsetMs || 0) - 10 });
      const offSpan = document.createElement('span');
      offSpan.style.minWidth = '48px';
      offSpan.style.textAlign = 'center';
      offSpan.textContent = `${Math.round(tel.userOffsetMs || 0)}`;
      const plusBtn = document.createElement('button');
      plusBtn.className = 'sm';
      plusBtn.textContent = '+10';
      plusBtn.onclick = () => updateListenerPrefs(l.id, { channelMode: tel.channelMode || 'stereo', userOffsetMs: (tel.userOffsetMs || 0) + 10 });
      offWrap.append(minusBtn, offSpan, plusBtn);
      offTd.appendChild(offWrap);

      const opTd = document.createElement('td');
      const kickBtn = document.createElement('button');
      kickBtn.className = 'sm danger';
      kickBtn.textContent = '移除';
      kickBtn.onclick = () => postJSON('/api/host/listener/kick', { id: l.id }).catch((e) => showError(e.message));
      opTd.appendChild(kickBtn);

      tr.append(nameTd, rttTd, jbTd, baseTd, compTd, lossTd, chTd, offTd, opTd);
      listenersBody.appendChild(tr);
    }
  }

  function updateListenerPrefs(id, prefs) {
    postJSON('/api/host/listener/prefs', { id, ...prefs }).catch((e) => showError(e.message));
  }

  function escapeHtml(s) {
    return String(s).replace(/[&<>"']/g, (c) => ({ '&': '&amp;', '<': '&lt;', '>': '&gt;', '"': '&quot;', "'": '&#39;' }[c]));
  }

  function patchConfig(patch, immediate = false) {
    if (immediate) {
      if (configDebounce) clearTimeout(configDebounce);
      configDebounce = null;
      return postJSON('/api/host/config', patch).catch((e) => showError(e.message));
    }
    if (configDebounce) clearTimeout(configDebounce);
    configDebounce = setTimeout(() => {
      configDebounce = null;
      postJSON('/api/host/config', patch).catch((e) => showError(e.message));
    }, 60);
  }

  toggleBroadcastBtn.addEventListener('click', () => {
    const active = state?.config?.broadcasting;
    const url = active ? '/api/host/stop' : '/api/host/start';
    postJSON(url, {}).catch((e) => showError(e.message));
  });

  switchTestToneBtn.addEventListener('click', () => {
    patchConfig({ sourceMode: 'test-tone' }, true);
  });

  sourceSegment.querySelectorAll('button').forEach((btn) => {
    btn.addEventListener('click', () => {
      patchConfig({ sourceMode: btn.dataset.source }, true);
    });
  });

  inputDeviceSelect.addEventListener('change', () => {
    patchConfig({ inputDeviceId: Number(inputDeviceSelect.value) }, true);
  });

  hostVolumeSlider.addEventListener('input', () => {
    hostVolumeVal.textContent = `${hostVolumeSlider.value}%`;
    patchConfig({ hostVolume: Number(hostVolumeSlider.value) / 100 });
  });

  bitrateSelect.addEventListener('change', () => {
    patchConfig({ bitrate: Number(bitrateSelect.value) }, true);
  });

  muteOriginalCheck.addEventListener('change', () => {
    patchConfig({ muteOriginal: muteOriginalCheck.checked }, true);
  });

  monitorEnableCheck.addEventListener('change', () => {
    patchConfig({ monitorEnabled: monitorEnableCheck.checked }, true);
  });

  monitorDeviceSelect.addEventListener('change', () => {
    patchConfig({ monitorDeviceId: Number(monitorDeviceSelect.value) }, true);
  });

  monitorGainSlider.addEventListener('input', () => {
    monitorGainVal.textContent = `${monitorGainSlider.value}%`;
    patchConfig({ monitorGain: Number(monitorGainSlider.value) / 100 });
  });

  hostOffsetSlider.addEventListener('input', () => {
    const v = Number(hostOffsetSlider.value);
    hostOffsetVal.textContent = `${v > 0 ? '+' : ''}${v} ms`;
    patchConfig({ hostUserOffsetMs: v });
  });

  qrModeSegment.querySelectorAll('button').forEach((btn) => {
    btn.addEventListener('click', () => {
      patchConfig({ qrMode: btn.dataset.mode }, true);
    });
  });

  ifaceSelect.addEventListener('change', () => {
    patchConfig({ selectedIface: ifaceSelect.value }, true);
  });

  copyUrlBtn.addEventListener('click', async () => {
    if (!currentJoinUrl) return;
    try {
      await navigator.clipboard.writeText(currentJoinUrl);
      const prev = copyUrlBtn.textContent;
      copyUrlBtn.textContent = '已复制';
      setTimeout(() => { copyUrlBtn.textContent = prev; }, 1200);
    } catch {
      showError('剪贴板复制失败，请手动选中复制');
    }
  });

  rotateTokenBtn.addEventListener('click', () => {
    patchConfig({ rotateToken: true }, true);
  });

  function connectHostWS() {
    const proto = window.location.protocol === 'https:' ? 'wss:' : 'ws:';
    const ws = new WebSocket(`${proto}//${window.location.host}/ws/host`);
    ws.onmessage = (evt) => {
      try {
        renderState(JSON.parse(evt.data));
      } catch {}
    };
    ws.onclose = () => {
      setTimeout(connectHostWS, 1500);
    };
  }

  fetch('/api/host/state')
    .then((r) => r.json())
    .then(renderState)
    .catch((e) => showError(e.message));
  connectHostWS();
})();
