(() => {
  const VOICE_ENDPOINT = '/webui/api/voice/token';
  const voiceSelect = document.getElementById('voiceSelect');
  const personalitySelect = document.getElementById('personalitySelect');
  const speedSelect = document.getElementById('speedSelect');
const instructionInput = document.getElementById('instructionInput');
  const instructionExpandBtn = document.getElementById('instructionExpandBtn');

  instructionExpandBtn?.addEventListener('click', () => {
    const overlay = document.createElement('div');
    overlay.className = 'webui-chatkit-modal-overlay';
    const dialog = document.createElement('div');
    dialog.className = 'webui-chatkit-modal-dialog';
    const textarea = document.createElement('textarea');
    textarea.className = 'webui-chatkit-modal-textarea';
    textarea.value = instructionInput?.value || '';
    textarea.placeholder = '输入自定义指令，Grokm Voice 会直接使用这段文字作为系统提示词';
    const actions = document.createElement('div');
    actions.className = 'webui-chatkit-modal-actions';
    const cancelBtn = document.createElement('button');
    cancelBtn.className = 'btn webui-chatkit-modal-btn-cancel';
    cancelBtn.textContent = '取消';
    const confirmBtn = document.createElement('button');
    confirmBtn.className = 'btn btn-primary webui-chatkit-modal-btn-confirm';
    confirmBtn.textContent = '确定';
    actions.append(cancelBtn, confirmBtn);
    dialog.append(textarea, actions);
    overlay.append(dialog);
    document.body.append(overlay);
    textarea.focus();
    const close = () => overlay.remove();
    cancelBtn.addEventListener('click', close);
    overlay.addEventListener('click', (e) => { if (e.target === overlay) close(); });
    confirmBtn.addEventListener('click', () => {
      if (instructionInput) instructionInput.value = textarea.value;
      close();
    });
  });
  const startVoiceBtn = document.getElementById('startVoiceBtn');
  const muteVoiceBtn = document.getElementById('muteVoiceBtn');
  const newSessionBtn = document.getElementById('newSessionBtn');
  const connectionBadge = document.getElementById('connectionBadge');
  const connectionText = document.getElementById('connectionText');
  const voiceOrb = document.getElementById('voiceOrb');
  const audioRoot = document.getElementById('audioRoot');

  let room = null;
  let micEnabled = true;
  let outputMuted = false;
  let orbAudioContext = null;
  const orbInputs = new Map();
  let orbFrame = 0;
  let orbLevel = 0;
  let orbBeat = 0;
  let orbMotionPhase = 0;
  const audioElements = new Set();
  let lastStatusState = '';
  let lastStatusLabel = '';
  let lastStatusDescription = '';

  const controlIcon = {
    start: '<svg viewBox="0 0 24 24" aria-hidden="true"><path d="M9 7.25 17 12l-8 4.75V7.25Z" fill="currentColor" stroke="none"/></svg>',
    pause: '<svg viewBox="0 0 24 24" aria-hidden="true"><path d="M9 6.5v11"/><path d="M15 6.5v11"/></svg>',
    mute: '<svg viewBox="0 0 24 24" aria-hidden="true"><path d="M5 10h3l4-4v12l-4-4H5z"/><path d="M16 9a4.5 4.5 0 0 1 0 6"/><path d="M18.5 6.5a8 8 0 0 1 0 11"/></svg>',
    unmute: '<svg viewBox="0 0 24 24" aria-hidden="true"><path d="M5 10h3l4-4v12l-4-4H5z"/><path d="m16 9 5 6"/><path d="m21 9-5 6"/></svg>',
    newSession: '<svg viewBox="0 0 24 24" aria-hidden="true"><path d="M12 5v14"/><path d="M5 12h14"/></svg>',
  };

  const text = (key, fallback) => {
    if (typeof window.t !== 'function') return fallback;
    const value = t(key);
    return value === key ? fallback : value;
  };

  const setOrbLevel = (level) => {
    orbLevel = Math.max(0, Math.min(1, level || 0));
    if (!voiceOrb) return;
    voiceOrb.style.setProperty('--chatkit-level', orbLevel.toFixed(3));
    voiceOrb.classList.toggle('is-speaking', voiceOrb.classList.contains('is-live') && orbLevel > 0.045);
  };

  const setOrbBeat = (value) => {
    orbBeat = Math.max(0, Math.min(1, value || 0));
    if (!voiceOrb) return;
    voiceOrb.style.setProperty('--chatkit-beat', orbBeat.toFixed(3));
  };

  const setOrbMotion = (level) => {
    if (!voiceOrb) return;
    const intensity = Math.max(0, Math.min(1, level || 0));
    const activeIntensity = intensity > 0.03 ? Math.max(intensity, 0.18) : 0;
    const distance = activeIntensity * 11;
    orbMotionPhase += 0.14 + activeIntensity * 0.18;
    const drift = (speed, amplitude, offset = 0) => Math.sin(orbMotionPhase * speed + offset) * amplitude * distance;

    voiceOrb.style.setProperty('--chatkit-drift-a-x', `${drift(0.92, 0.95, 0.2).toFixed(2)}px`);
    voiceOrb.style.setProperty('--chatkit-drift-a-y', `${drift(1.08, 0.9, 1.1).toFixed(2)}px`);
    voiceOrb.style.setProperty('--chatkit-drift-b-x', `${drift(1.16, 0.82, 2.4).toFixed(2)}px`);
    voiceOrb.style.setProperty('--chatkit-drift-b-y', `${drift(0.98, 0.76, 0.7).toFixed(2)}px`);
    voiceOrb.style.setProperty('--chatkit-drift-c-x', `${drift(1.04, 1.02, 3.1).toFixed(2)}px`);
    voiceOrb.style.setProperty('--chatkit-drift-c-y', `${drift(1.22, 0.84, 1.8).toFixed(2)}px`);
    voiceOrb.style.setProperty('--chatkit-drift-d-x', `${drift(0.88, 0.78, 4.2).toFixed(2)}px`);
    voiceOrb.style.setProperty('--chatkit-drift-d-y', `${drift(1.14, 0.74, 2.7).toFixed(2)}px`);
    voiceOrb.style.setProperty('--chatkit-drift-e-x', `${drift(1.28, 0.62, 5.1).toFixed(2)}px`);
    voiceOrb.style.setProperty('--chatkit-drift-e-y', `${drift(0.94, 0.58, 3.5).toFixed(2)}px`);
  };

  const disconnectOrbInput = (entry) => {
    if (!entry) return;
    if (entry.source) {
      try {
        entry.source.disconnect();
      } catch {}
    }
  };

  const ensureOrbAudioContext = async () => {
    const AudioContextCtor = window.AudioContext || window.webkitAudioContext;
    if (!AudioContextCtor) return null;
    if (!orbAudioContext) orbAudioContext = new AudioContextCtor();
    if (orbAudioContext.state === 'suspended') {
      try {
        await orbAudioContext.resume();
      } catch {}
    }
    return orbAudioContext;
  };

  const measureOrbInput = (analyser, data) => {
    analyser.getByteTimeDomainData(data);
    let sum = 0;
    for (let index = 0; index < data.length; index += 1) {
      const normalized = (data[index] - 128) / 128;
      sum += normalized * normalized;
    }
    const rms = Math.sqrt(sum / data.length);
    return Math.max(0, Math.min(1, (rms - 0.01) * 14));
  };

  const stopOrbLoop = () => {
    if (!orbFrame) return;
    cancelAnimationFrame(orbFrame);
    orbFrame = 0;
  };

  const stopOrbAnalysis = () => {
    stopOrbLoop();
    orbInputs.forEach(disconnectOrbInput);
    orbInputs.clear();
    setOrbLevel(0);
    setOrbBeat(0);
  };

  const startOrbLoop = () => {
    if (orbFrame) return;
    const render = () => {
      let strongest = 0;
      let total = 0;
      orbInputs.forEach((entry) => {
        const level = measureOrbInput(entry.analyser, entry.data);
        strongest = Math.max(strongest, level);
        total += level;
      });

      const targetLevel = orbInputs.size
        ? Math.max(strongest, Math.min(1, strongest * 0.86 + total * 0.12))
        : 0;
      const smoothing = targetLevel > orbLevel ? 0.38 : 0.18;
      const nextLevel = orbLevel + (targetLevel - orbLevel) * smoothing;
      const normalizedLevel = nextLevel < 0.006 ? 0 : nextLevel;
      const beatTarget = orbInputs.size ? Math.min(1, strongest * 1.35) : 0;
      const beatSmoothing = beatTarget > orbBeat ? 0.58 : 0.14;
      const nextBeat = orbBeat + (beatTarget - orbBeat) * beatSmoothing;
      setOrbLevel(normalizedLevel);
      setOrbBeat(nextBeat < 0.01 ? 0 : nextBeat);
      setOrbMotion(normalizedLevel);

      if (!orbInputs.size && normalizedLevel < 0.006) {
        orbFrame = 0;
        return;
      }
      orbFrame = requestAnimationFrame(render);
    };
    orbFrame = requestAnimationFrame(render);
  };

  const removeOrbInput = (key) => {
    const entry = orbInputs.get(key);
    if (!entry) return;
    disconnectOrbInput(entry);
    orbInputs.delete(key);
    if (!orbInputs.size) stopOrbLoop();
  };

  const removeOrbInputsByPrefix = (prefix) => {
    Array.from(orbInputs.keys()).forEach((key) => {
      if (key.startsWith(prefix)) removeOrbInput(key);
    });
  };

  const attachOrbStream = async (stream, key) => {
    if (!(stream instanceof MediaStream) || !key || orbInputs.has(key)) return;

    const context = await ensureOrbAudioContext();
    if (!context) return;

    try {
      const analyser = context.createAnalyser();
      analyser.fftSize = 256;
      analyser.smoothingTimeConstant = 0.72;
      const data = new Uint8Array(analyser.fftSize);
      const source = context.createMediaStreamSource(stream);
      source.connect(analyser);
      orbInputs.set(key, { analyser, data, source });
      startOrbLoop();
    } catch {}
  };

  const getMediaStreamTrack = (trackOrPublication) => {
    const candidate = trackOrPublication?.track || trackOrPublication;
    const mediaTrack = candidate?.mediaStreamTrack
      || candidate?.track?.mediaStreamTrack
      || candidate?._mediaStreamTrack
      || null;
    if (!mediaTrack || mediaTrack.kind !== 'audio' || mediaTrack.readyState !== 'live') return null;
    return mediaTrack;
  };

  const syncLocalMicAnalysis = async (currentRoom) => {
    removeOrbInputsByPrefix('local:');
    if (!currentRoom || !micEnabled) return;

    const publications = currentRoom.localParticipant?.trackPublications;
    if (!publications || typeof publications.values !== 'function') return;

    for (const publication of publications.values()) {
      const mediaTrack = getMediaStreamTrack(publication);
      if (!mediaTrack || mediaTrack.enabled === false) continue;
      const stream = new MediaStream([mediaTrack]);
      await attachOrbStream(stream, `local:${mediaTrack.id || 'mic'}`);
      break;
    }
  };

  const setStatus = (state, label, description) => {
    if (connectionBadge && lastStatusLabel !== label) connectionBadge.textContent = label;
    if (connectionBadge && lastStatusState !== state) connectionBadge.dataset.state = state;
    if (connectionText && lastStatusDescription !== description) connectionText.textContent = description;
    lastStatusState = state;
    lastStatusLabel = label;
    lastStatusDescription = description;
    if (voiceOrb) {
      if (!voiceOrb.classList.contains(state)) {
        voiceOrb.classList.remove('is-idle', 'is-connecting', 'is-live', 'is-paused', 'is-output-muted', 'is-error');
        voiceOrb.classList.add(state);
      }
      if (state !== 'is-live') {
        voiceOrb.classList.remove('is-speaking');
        setOrbLevel(0);
        setOrbBeat(0);
      }
    }
  };

  const renderConnectedStatus = () => {
    if (!room) {
      setStatus(
        'is-idle',
        text('webui.chatkit.statusIdle', '未连接'),
        text('webui.chatkit.idleText', '点击并授权，通过 ChatKit 语音会话连接 Grok Voice。'),
      );
      return;
    }

    if (!micEnabled) {
      setStatus(
        'is-paused',
        text('webui.chatkit.statusPaused', '已暂停'),
        text('webui.chatkit.pausedText', '会话已暂停，点击开始即可继续当前语音会话。'),
      );
      return;
    }

    if (outputMuted) {
      setStatus(
        'is-output-muted',
        text('webui.chatkit.statusMuted', '已静音'),
        text('webui.chatkit.outputMutedText', '扬声器已静音，你仍然可以继续说话。'),
      );
      return;
    }

    setStatus(
      'is-live',
      text('webui.chatkit.statusLive', '语音中'),
      text('webui.chatkit.liveText', '连接已建立，现在可以直接开口和 Grok 对话。'),
    );
  };

  const setButtons = (connected) => {
    if (startVoiceBtn) {
      startVoiceBtn.disabled = false;
      const label = connected && micEnabled
        ? text('webui.chatkit.pause', '暂停')
        : text('webui.chatkit.start', '开始');
      startVoiceBtn.innerHTML = connected && micEnabled ? controlIcon.pause : controlIcon.start;
      startVoiceBtn.setAttribute('aria-label', label);
      startVoiceBtn.setAttribute('title', label);
    }
    if (muteVoiceBtn) {
      muteVoiceBtn.disabled = !connected;
      const label = outputMuted
        ? text('webui.chatkit.unmute', '取消静音')
        : text('webui.chatkit.mute', '静音');
      muteVoiceBtn.innerHTML = outputMuted ? controlIcon.unmute : controlIcon.mute;
      muteVoiceBtn.setAttribute('aria-label', label);
      muteVoiceBtn.setAttribute('title', label);
    }
    if (newSessionBtn) {
      newSessionBtn.disabled = !connected;
      const label = text('webui.chatkit.newSession', '新会话');
      newSessionBtn.innerHTML = controlIcon.newSession;
      newSessionBtn.setAttribute('aria-label', label);
      newSessionBtn.setAttribute('title', label);
    }
  };

  const detachAudio = () => {
    stopOrbAnalysis();
    audioElements.forEach((node) => {
      try {
        node.pause();
        node.srcObject = null;
      } catch {}
      node.remove();
    });
    audioElements.clear();
  };

  const getLiveKit = () => window.LiveKitClient || window.LivekitClient || null;

  const getAuthHeaders = async () => {
    const key = await webuiKey.get();
    return key ? { Authorization: `Bearer ${key}` } : {};
  };

  const addRemoteAudioTrack = (track) => {
    if (!audioRoot || !track || track.kind !== 'audio') return;
    const element = track.attach();
    element.autoplay = true;
    element.playsInline = true;
    element.muted = outputMuted;
    audioRoot.appendChild(element);
    audioElements.add(element);
    const stream = element.srcObject instanceof MediaStream ? element.srcObject : null;
    const streamId = stream?.id || stream?.getAudioTracks?.()[0]?.id || '';
    if (stream && streamId) {
      void attachOrbStream(stream, `remote:${streamId}`);
    }
  };

  const bindRoomEvents = (lk, currentRoom) => {
    currentRoom.on(lk.RoomEvent.TrackSubscribed, (track) => {
      addRemoteAudioTrack(track);
    });

    currentRoom.on(lk.RoomEvent.TrackUnsubscribed, (track) => {
      try {
        const elements = track.detach();
        elements.forEach((el) => {
          let streamId = '';
          if (el instanceof HTMLMediaElement && el.srcObject instanceof MediaStream) {
            streamId = el.srcObject.id || el.srcObject.getAudioTracks?.()[0]?.id || '';
          }
          if (streamId) removeOrbInput(`remote:${streamId}`);
          if (el instanceof HTMLAudioElement) audioElements.delete(el);
          el.remove();
        });
      } catch {}
    });

    currentRoom.on(lk.RoomEvent.Disconnected, () => {
      teardownSession(false);
    });
  };

  const teardownSession = async (manual) => {
    const currentRoom = room;
    room = null;
    try {
      if (currentRoom) await currentRoom.disconnect();
    } catch {}
    detachAudio();
    micEnabled = true;
    outputMuted = false;
    setButtons(false);
    renderConnectedStatus();
    if (manual && connectionText) {
      connectionText.textContent = text('webui.chatkit.endedText', '语音会话已结束，可以重新开始。');
    }
  };

  const startSession = async () => {
    const lk = getLiveKit();
    if (!lk || !lk.Room) {
      showToast?.(text('webui.chatkit.livekitLoadFailed', 'LiveKit SDK 加载失败'), 'error');
      return;
    }

    if (startVoiceBtn) startVoiceBtn.disabled = true;
    void ensureOrbAudioContext();
    setStatus(
      'is-connecting',
      text('webui.chatkit.statusConnecting', '正在连接'),
      text('webui.chatkit.connectingText', '正在向 Grok Voice 申请会话并连接 LiveKit…'),
    );

    try {
const params = new URLSearchParams({
        voice: voiceSelect?.value || 'ara',
        personality: personalitySelect?.value || 'assistant',
        speed: speedSelect?.value || '1.0',
        instruction: instructionInput?.value?.trim() || '',
      });
      const res = await fetch(`${VOICE_ENDPOINT}?${params.toString()}`, {
        headers: await getAuthHeaders(),
        cache: 'no-store',
      });
      if (!res.ok) {
        const detail = await res.text().catch(() => '');
        throw new Error(detail || `HTTP ${res.status}`);
      }

      const payload = await res.json();
      if (!payload || !payload.token || !payload.url) {
        throw new Error(text('webui.chatkit.invalidToken', 'Voice token response invalid'));
      }

      const currentRoom = new lk.Room({
        adaptiveStream: true,
        dynacast: true,
        audioCaptureDefaults: {
          autoGainControl: true,
          echoCancellation: true,
          noiseSuppression: true,
        },
      });
      room = currentRoom;
      bindRoomEvents(lk, currentRoom);

      await currentRoom.connect(payload.url, payload.token);
      await currentRoom.localParticipant.setMicrophoneEnabled(true);

      micEnabled = true;
      outputMuted = false;
      setButtons(true);
      renderConnectedStatus();
      void syncLocalMicAnalysis(currentRoom);
    } catch (error) {
      const message = error && error.message ? error.message : String(error);
      showToast?.(message, 'error');
      setStatus(
        'is-error',
        text('webui.chatkit.statusError', '连接失败'),
        text('webui.chatkit.errorText', '连接没有建立成功，请检查麦克风权限后重试。'),
      );
      await teardownSession(false);
    } finally {
      if (startVoiceBtn && !room) startVoiceBtn.disabled = false;
    }
  };

  const togglePause = async () => {
    if (!room) return;
    micEnabled = !micEnabled;
    await room.localParticipant.setMicrophoneEnabled(micEnabled);
    setButtons(true);
    renderConnectedStatus();
    void syncLocalMicAnalysis(room);
  };

  const toggleOutputMute = () => {
    if (!room) return;
    outputMuted = !outputMuted;
    audioElements.forEach((node) => {
      node.muted = outputMuted;
    });
    setButtons(true);
    renderConnectedStatus();
  };

  const handlePrimaryAction = async () => {
    if (!room) {
      await startSession();
      return;
    }
    await togglePause();
  };

  const startFreshSession = async () => {
    if (!room) return;
    await teardownSession(true);
    await startSession();
  };

  startVoiceBtn?.addEventListener('click', () => {
    void handlePrimaryAction();
  });
  muteVoiceBtn?.addEventListener('click', toggleOutputMute);
  newSessionBtn?.addEventListener('click', () => {
    void startFreshSession();
  });

  window.addEventListener('beforeunload', () => {
    if (room) void room.disconnect();
  });

  setButtons(false);
  setStatus(
    'is-idle',
    text('webui.chatkit.statusIdle', '未连接'),
    text('webui.chatkit.idleText', '点击并授权，通过 ChatKit 语音会话连接 Grok Voice。'),
  );
  if (typeof renderWebuiHeader === 'function') {
    void renderWebuiHeader();
  }
  if (typeof renderSiteFooter === 'function') {
    void renderSiteFooter();
  }
})();
