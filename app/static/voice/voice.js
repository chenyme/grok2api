(() => {
  let Room;
  let createLocalTracks;
  let RoomEvent;
  let Track;
  let room = null;
  let visualizerTimer = null;

  const startBtn = document.getElementById('startBtn');
  const stopBtn = document.getElementById('stopBtn');
  const statusText = document.getElementById('statusText');
  const logContainer = document.getElementById('log');
  const voiceSelect = document.getElementById('voiceSelect');
  const personalitySelect = document.getElementById('personalitySelect');
  const speedRange = document.getElementById('speedRange');
  const speedValue = document.getElementById('speedValue');
  const statusVoice = document.getElementById('statusVoice');
  const statusPersonality = document.getElementById('statusPersonality');
  const statusSpeed = document.getElementById('statusSpeed');
  const audioRoot = document.getElementById('audioRoot');
  const copyLogBtn = document.getElementById('copyLogBtn');
  const clearLogBtn = document.getElementById('clearLogBtn');
  const visualizer = document.getElementById('visualizer');

  function log(message, level = 'info') {
    if (!logContainer) {
      return;
    }
    const p = document.createElement('p');
    const time = new Date().toLocaleTimeString();
    p.textContent = `[${time}] ${message}`;
    if (level === 'error') {
      p.classList.add('log-error');
    } else if (level === 'warn') {
      p.classList.add('log-warn');
    }
    logContainer.prepend(p);
    if (typeof console !== 'undefined') {
      console.log(message);
    }
  }

  function toast(message, type) {
    if (typeof showToast === 'function') {
      showToast(message, type);
    } else {
      log(message, type === 'error' ? 'error' : 'info');
    }
  }

  function setStatus(state, text) {
    if (!statusText) {
      return;
    }
    statusText.textContent = text;
    statusText.classList.remove('connected', 'connecting', 'error');
    if (state) {
      statusText.classList.add(state);
    }
  }

  function setButtons(connected) {
    if (!startBtn || !stopBtn) {
      return;
    }
    if (connected) {
      startBtn.classList.add('hidden');
      stopBtn.classList.remove('hidden');
    } else {
      startBtn.classList.remove('hidden');
      stopBtn.classList.add('hidden');
      startBtn.disabled = false;
    }
  }

  function updateMeta() {
    if (statusVoice) {
      statusVoice.textContent = voiceSelect.value;
    }
    if (statusPersonality) {
      statusPersonality.textContent = personalitySelect.value;
    }
    if (statusSpeed) {
      statusSpeed.textContent = `${speedRange.value}x`;
    }
  }

  function initLiveKit() {
    const lk = window.LiveKitClient || window.LivekitClient;
    if (!lk) {
      return false;
    }
    Room = lk.Room;
    createLocalTracks = lk.createLocalTracks;
    RoomEvent = lk.RoomEvent;
    Track = lk.Track;
    return true;
  }

  function ensureLiveKit() {
    if (Room) {
      return true;
    }
    if (!initLiveKit()) {
      log('错误: LiveKit SDK 未能正确加载，请刷新页面重试', 'error');
      toast('LiveKit SDK 加载失败', 'error');
      return false;
    }
    return true;
  }

  function ensureMicSupport() {
    const hasMediaDevices = typeof navigator !== 'undefined' && navigator.mediaDevices;
    const hasGetUserMedia = hasMediaDevices && typeof navigator.mediaDevices.getUserMedia === 'function';
    if (hasGetUserMedia) {
      return true;
    }
    const isLocalhost = ['localhost', '127.0.0.1'].includes(window.location.hostname);
    const secureHint = window.isSecureContext || isLocalhost
      ? '请使用最新版浏览器并允许麦克风权限'
      : '请使用 HTTPS 或在本机 localhost 访问';
    throw new Error(`当前环境不支持麦克风权限，${secureHint}`);
  }

  async function startSession() {
    if (!ensureLiveKit()) {
      return;
    }

    try {
      const apiKey = await ensureApiKey();
      if (apiKey === null) {
        toast('请先登录后台', 'error');
        return;
      }

      startBtn.disabled = true;
      updateMeta();
      setStatus('connecting', '正在连接');
      log('正在获取 Token...');

      const params = new URLSearchParams({
        voice: voiceSelect.value,
        personality: personalitySelect.value,
        speed: speedRange.value
      });

      const headers = buildAuthHeaders(apiKey);

      const response = await fetch(`/api/v1/admin/voice/token?${params.toString()}`, {
        headers
      });

      if (!response.ok) {
        throw new Error(`获取 Token 失败: ${response.status}`);
      }

      const { token, url } = await response.json();
      log(`获取 Token 成功 (${voiceSelect.value}, ${personalitySelect.value}, ${speedRange.value}x)`);

      room = new Room({
        adaptiveStream: true,
        dynacast: true
      });

      room.on(RoomEvent.ParticipantConnected, (p) => log(`参与者已连接: ${p.identity}`));
      room.on(RoomEvent.ParticipantDisconnected, (p) => log(`参与者已断开: ${p.identity}`));
      room.on(RoomEvent.TrackSubscribed, (track) => {
        log(`订阅音轨: ${track.kind}`);
        if (track.kind === Track.Kind.Audio) {
          const element = track.attach();
          if (audioRoot) {
            audioRoot.appendChild(element);
          } else {
            document.body.appendChild(element);
          }
        }
      });

      room.on(RoomEvent.Disconnected, () => {
        log('已断开连接');
        resetUI();
      });

      await room.connect(url, token);
      log('已连接到 LiveKit 服务器');

      setStatus('connected', '通话中');
      setButtons(true);

      log('正在开启麦克风...');
      ensureMicSupport();
      const tracks = await createLocalTracks({ audio: true, video: false });
      for (const track of tracks) {
        await room.localParticipant.publishTrack(track);
      }
      log('语音已开启');
      toast('语音连接成功', 'success');
    } catch (err) {
      const message = err && err.message ? err.message : '连接失败';
      log(`错误: ${message}`, 'error');
      toast(message, 'error');
      setStatus('error', '连接错误');
      startBtn.disabled = false;
    }
  }

  async function stopSession() {
    if (room) {
      await room.disconnect();
    }
    resetUI();
  }

  function resetUI() {
    setStatus('', '未连接');
    setButtons(false);
    if (audioRoot) {
      audioRoot.innerHTML = '';
    }
  }

  function clearLog() {
    if (logContainer) {
      logContainer.innerHTML = '';
    }
  }

  async function copyLog() {
    if (!logContainer) {
      return;
    }
    const lines = Array.from(logContainer.querySelectorAll('p'))
      .map((p) => p.textContent)
      .join('\n');
    try {
      await navigator.clipboard.writeText(lines);
      toast('日志已复制', 'success');
    } catch (err) {
      toast('复制失败，请手动选择', 'error');
    }
  }

  speedRange.addEventListener('input', (e) => {
    speedValue.textContent = Number(e.target.value).toFixed(1);
    const min = Number(speedRange.min || 0);
    const max = Number(speedRange.max || 100);
    const val = Number(speedRange.value || 0);
    const pct = ((val - min) / (max - min)) * 100;
    speedRange.style.setProperty('--range-progress', `${pct}%`);
    updateMeta();
  });

  voiceSelect.addEventListener('change', updateMeta);
  personalitySelect.addEventListener('change', updateMeta);

  startBtn.addEventListener('click', startSession);
  stopBtn.addEventListener('click', stopSession);
  if (copyLogBtn) {
    copyLogBtn.addEventListener('click', copyLog);
  }
  if (clearLogBtn) {
    clearLogBtn.addEventListener('click', clearLog);
  }

  speedValue.textContent = Number(speedRange.value).toFixed(1);
  {
    const min = Number(speedRange.min || 0);
    const max = Number(speedRange.max || 100);
    const val = Number(speedRange.value || 0);
    const pct = ((val - min) / (max - min)) * 100;
    speedRange.style.setProperty('--range-progress', `${pct}%`);
  }
  function buildVisualizerBars() {
    if (!visualizer) return;
    visualizer.innerHTML = '';
    const targetCount = Math.max(36, Math.floor(visualizer.offsetWidth / 7));
    for (let i = 0; i < targetCount; i += 1) {
      const bar = document.createElement('div');
      bar.className = 'bar';
      visualizer.appendChild(bar);
    }
  }

  window.addEventListener('resize', buildVisualizerBars);
  buildVisualizerBars();
  updateMeta();
  setStatus('', '未连接');

  if (!visualizerTimer) {
    visualizerTimer = setInterval(() => {
      const bars = document.querySelectorAll('.visualizer .bar');
      bars.forEach((bar) => {
        if (statusText && statusText.classList.contains('connected')) {
          bar.style.height = `${Math.random() * 32 + 6}px`;
        } else {
          bar.style.height = '6px';
        }
      });
    }, 150);
  }
})();
