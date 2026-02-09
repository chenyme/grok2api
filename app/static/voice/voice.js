let __voiceCleanup = null;

function _setupVoicePage() {
  const LIVEKIT_SDK_URL = 'https://cdn.jsdelivr.net/npm/livekit-client@2.7.3/dist/livekit-client.umd.min.js';

  let Room;
  let createLocalTracks;
  let RoomEvent;
  let Track;
  let room = null;
  let visualizerTimer = null;
  let livekitLoadPromise = null;
  const audioElementMap = new Map();

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

  if (!startBtn || !stopBtn || !statusText || !voiceSelect || !personalitySelect || !speedRange || !speedValue) {
    return () => {};
  }

  let onSpeedInput = null;
  let onResize = null;

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

  function getLiveKitGlobal() {
    return window.LiveKitClient || window.LivekitClient || window.livekitClient;
  }

  function initLiveKit() {
    const lk = getLiveKitGlobal();
    if (!lk) {
      return false;
    }
    Room = lk.Room;
    createLocalTracks = lk.createLocalTracks;
    RoomEvent = lk.RoomEvent;
    Track = lk.Track;
    return true;
  }

  async function loadLiveKitSdk() {
    if (getLiveKitGlobal()) {
      return;
    }
    if (livekitLoadPromise) {
      await livekitLoadPromise;
      return;
    }

    livekitLoadPromise = new Promise((resolve, reject) => {
      const existing = document.querySelector('script[data-livekit-sdk="1"]');
      if (existing) {
        if (getLiveKitGlobal()) {
          resolve();
          return;
        }
        existing.addEventListener('load', () => resolve(), { once: true });
        existing.addEventListener('error', () => reject(new Error('LiveKit SDK 加载失败')), { once: true });
        return;
      }

      const script = document.createElement('script');
      script.src = LIVEKIT_SDK_URL;
      script.async = true;
      script.dataset.livekitSdk = '1';
      script.onload = () => resolve();
      script.onerror = () => reject(new Error('LiveKit SDK 加载失败'));
      document.head.appendChild(script);
    });

    await livekitLoadPromise;
  }

  async function ensureLiveKit() {
    if (Room) {
      return true;
    }

    if (!initLiveKit()) {
      try {
        await loadLiveKitSdk();
      } catch (err) {
        log('错误: LiveKit SDK 动态加载失败', 'error');
        toast('LiveKit SDK 加载失败', 'error');
        return false;
      }
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

  async function attachAudioTrack(track, keyHint = '') {
    if (!track || !Track || track.kind !== Track.Kind.Audio) {
      return;
    }

    const key = track.sid || keyHint || `audio-${Date.now()}-${Math.random().toString(36).slice(2, 8)}`;
    if (audioElementMap.has(key)) {
      return;
    }

    const element = track.attach();
    element.autoplay = true;
    element.playsInline = true;
    element.controls = true;
    element.muted = false;
    element.volume = 1;

    if (audioRoot) {
      audioRoot.appendChild(element);
    } else {
      document.body.appendChild(element);
    }

    audioElementMap.set(key, element);
    try {
      await element.play();
    } catch (err) {
      log('远端音频等待浏览器授权播放，可尝试再次点击“开始会话”', 'warn');
    }
  }

  function detachAudioTrack(track, keyHint = '') {
    const key = (track && track.sid) || keyHint;

    try {
      if (track && typeof track.detach === 'function') {
        const detached = track.detach();
        if (Array.isArray(detached)) {
          detached.forEach((el) => {
            try {
              el.remove();
            } catch (e) {
              // ignore
            }
          });
        }
      }
    } catch (e) {
      // ignore
    }

    if (key && audioElementMap.has(key)) {
      const el = audioElementMap.get(key);
      try {
        el.pause();
      } catch (e) {
        // ignore
      }
      try {
        el.remove();
      } catch (e) {
        // ignore
      }
      audioElementMap.delete(key);
    }
  }

  async function startSession() {
    if (!(await ensureLiveKit())) {
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
      room.on(RoomEvent.TrackSubscribed, (track, publication, participant) => {
        log(`订阅音轨: ${track.kind}`);
        const keyHint = (publication && (publication.trackSid || publication.sid)) || (participant && participant.identity) || '';
        attachAudioTrack(track, keyHint).catch(() => {});
      });
      room.on(RoomEvent.TrackUnsubscribed, (track, publication, participant) => {
        const keyHint = (publication && (publication.trackSid || publication.sid)) || (participant && participant.identity) || '';
        detachAudioTrack(track, keyHint);
      });

      if (RoomEvent.AudioPlaybackStatusChanged) {
        room.on(RoomEvent.AudioPlaybackStatusChanged, (canPlaybackAudio) => {
          if (!canPlaybackAudio) {
            log('浏览器阻止了自动播放，请点击页面后重试', 'warn');
          }
        });
      }

      room.on(RoomEvent.Disconnected, () => {
        log('已断开连接');
        resetUI();
      });

      await room.connect(url, token);
      log('已连接到 LiveKit 服务器');

      if (typeof room.startAudio === 'function') {
        const started = await room.startAudio();
        if (!started) {
          log('音频播放尚未激活，请与页面交互后重试', 'warn');
        }
      }

      for (const participant of room.remoteParticipants.values()) {
        participant.trackPublications.forEach((publication) => {
          if (publication && publication.track && publication.kind === Track.Kind.Audio) {
            const keyHint = publication.trackSid || publication.sid || participant.identity || '';
            attachAudioTrack(publication.track, keyHint).catch(() => {});
          }
        });
      }

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
      room = null;
    }
    resetUI();
  }

  function resetUI() {
    setStatus('', '未连接');
    setButtons(false);
    if (audioRoot) {
      audioRoot.innerHTML = '';
    }
    audioElementMap.forEach((el) => {
      try {
        el.pause();
      } catch (e) {
        // ignore
      }
      try {
        el.remove();
      } catch (e) {
        // ignore
      }
    });
    audioElementMap.clear();
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

  onSpeedInput = (e) => {
    speedValue.textContent = Number(e.target.value).toFixed(1);
    const min = Number(speedRange.min || 0);
    const max = Number(speedRange.max || 100);
    const val = Number(speedRange.value || 0);
    const pct = ((val - min) / (max - min)) * 100;
    speedRange.style.setProperty('--range-progress', `${pct}%`);
    updateMeta();
  };
  speedRange.addEventListener('input', onSpeedInput);

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

  onResize = buildVisualizerBars;
  window.addEventListener('resize', onResize);
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

  const cleanup = () => {
    if (onSpeedInput) {
      speedRange.removeEventListener('input', onSpeedInput);
    }
    if (onResize) {
      window.removeEventListener('resize', onResize);
    }
    if (visualizerTimer) {
      clearInterval(visualizerTimer);
      visualizerTimer = null;
    }
    stopSession().catch(() => {});
  };

  return cleanup;
}

function initVoicePage() {
  if (__voiceCleanup) {
    try {
      __voiceCleanup();
    } catch (e) {
      // ignore cleanup errors
    }
  }
  __voiceCleanup = _setupVoicePage();
}

function cleanupVoicePage() {
  if (!__voiceCleanup) return;
  try {
    __voiceCleanup();
  } catch (e) {
    // ignore cleanup errors
  }
  __voiceCleanup = null;
}

window.GrokAdminPages = window.GrokAdminPages || {};
window.GrokAdminPages.voice = {
  init: initVoicePage,
  cleanup: cleanupVoicePage,
};

if (window.__GROK_ADMIN_SPA__ !== true) {
  initVoicePage();
}
