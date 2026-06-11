/*
 * ElectronStudio 前端逻辑（纯原生 JS，无依赖、无构建）。
 *
 * 职责：维护一条到后端的 WebSocket，按 internal/protocol 的契约收发消息，
 * 并据此更新界面。消息类型常量与 web/src/protocol.ts 保持一致。
 */
(() => {
  'use strict';

  // ---- 协议常量（镜像自 internal/protocol）----
  const SrvType = {
    Status: 'status', VoiceState: 'voice_state', VAD: 'vad', Wake: 'wake',
    ASR: 'asr', Chat: 'chat', TTS: 'tts', Emotion: 'emotion',
    Joints: 'joints', Error: 'error', Log: 'log',
  };
  const CliType = {
    SendText: 'send_text', Mic: 'mic', Interrupt: 'interrupt',
    PlayAction: 'play_action', SetEmotion: 'set_emotion',
    SelectModel: 'select_model', JogJoint: 'jog_joint',
  };

  // ---- DOM 引用 ----
  const $ = (id) => document.getElementById(id);
  const el = {
    conn: $('conn-state'),
    dotUSB: $('dot-usb'), dotASR: $('dot-asr'), dotTTS: $('dot-tts'),
    model: $('model-select'),
    face: $('robot-face'), faceFallback: $('face-fallback'), mirror: $('mirror'),
    voiceState: $('voice-state'), vsText: $('vs-text'),
    waveform: $('waveform'),
    chat: $('chat-stream'),
    composer: $('composer'), input: $('composer-input'),
    mic: $('btn-mic'), interrupt: $('btn-interrupt'),
    toast: $('toast'),
  };

  const VOICE_LABEL = { idle: '待命', connecting: '连接中', listening: '聆听中…', thinking: '思考中…', speaking: '回应中…' };

  // ---- WebSocket 连接（带自动重连）----
  let ws = null;
  let reconnectTimer = null;

  function connect() {
    const proto = location.protocol === 'https:' ? 'wss' : 'ws';
    ws = new WebSocket(`${proto}://${location.host}/ws`);
    ws.binaryType = 'arraybuffer';

    ws.onopen = () => setConn('已连接', 'ok');
    ws.onclose = () => {
      setConn('已断开 · 重连中', 'off');
      clearTimeout(reconnectTimer);
      reconnectTimer = setTimeout(connect, 1500); // 简单退避重连
    };
    ws.onerror = () => ws.close();
    ws.onmessage = (e) => {
      if (typeof e.data === 'string') handleEnvelope(e.data);
      else handleFrame(e.data); // 二进制 = 屏幕镜像帧
    };
  }

  function send(type, payload) {
    if (!ws || ws.readyState !== WebSocket.OPEN) return;
    ws.send(JSON.stringify({ type, ts: Date.now(), payload }));
  }

  function setConn(text, cls) {
    el.conn.textContent = text;
    el.conn.className = 'conn-state ' + (cls || '');
  }

  // ---- 入站消息分发 ----
  function handleEnvelope(raw) {
    let env;
    try { env = JSON.parse(raw); } catch { return; }
    const p = env.payload || {};
    switch (env.type) {
      case SrvType.Status: onStatus(p); break;
      case SrvType.VoiceState: onVoiceState(p); break;
      case SrvType.VAD: onVAD(p); break;
      case SrvType.Wake: setVoice('listening'); break;
      case SrvType.ASR: /* 可在此显示实时识别字幕，从略 */ break;
      case SrvType.Chat: onChat(p); break;
      case SrvType.Emotion: el.face.dataset.emotion = p.emotion || 'neutral'; break;
      case SrvType.TTS: break; // 语音播放状态，当前由 voice_state 体现
      case SrvType.Joints: break; // 舵机反馈，编排页会用到
      case SrvType.Error: toast(p.message || '发生错误'); break;
      default: break;
    }
  }

  function onStatus(s) {
    toggleDot(el.dotUSB, s.robot && s.robot.connected);
    toggleDot(el.dotASR, s.asr && s.asr.running);
    toggleDot(el.dotTTS, s.tts && s.tts.running);
    // 模型下拉。
    if (s.llm) {
      el.model.innerHTML = '';
      (s.llm.available || []).forEach((m) => {
        const opt = document.createElement('option');
        opt.value = m.id; opt.textContent = m.name;
        if (m.id === s.llm.active) opt.selected = true;
        el.model.appendChild(opt);
      });
    }
  }

  function toggleDot(node, on) { node.classList.toggle('on', !!on); }

  function onVoiceState(p) { setVoice(p.state || 'idle'); }
  function setVoice(state) {
    el.face.dataset.state = state;
    el.voiceState.dataset.state = state;
    el.vsText.textContent = VOICE_LABEL[state] || state;
  }

  // ---- 对话流：按 id 去重/更新，支持流式 ----
  const msgNodes = new Map();
  function onChat(p) {
    let node = msgNodes.get(p.id);
    if (!node) {
      node = document.createElement('div');
      node.className = `msg ${p.role}`;
      node.innerHTML = `<div class="role">${p.role === 'user' ? '我' : '小电'}</div><div class="content"></div>`;
      el.chat.appendChild(node);
      msgNodes.set(p.id, node);
    }
    node.classList.toggle('streaming', p.status === 'streaming');
    node.querySelector('.content').textContent = p.content || '';
    // 工具调用徽章。
    if (p.tools && p.tools.length) {
      node.querySelectorAll('.tool-badge').forEach((n) => n.remove());
      p.tools.forEach((t) => {
        const b = document.createElement('span');
        b.className = 'tool-badge';
        b.textContent = `🔧 ${t.name}${t.status ? ' · ' + t.status : ''}`;
        node.appendChild(b);
      });
    }
    el.chat.scrollTop = el.chat.scrollHeight;
  }

  // ---- VAD 波形 ----
  const waveCtx = el.waveform.getContext('2d');
  const waveBuf = new Array(60).fill(0);
  function onVAD(p) {
    waveBuf.push(p.speaking ? (p.level || 0.5) : 0);
    waveBuf.shift();
    drawWave();
  }
  function drawWave() {
    const w = el.waveform.width, h = el.waveform.height;
    waveCtx.clearRect(0, 0, w, h);
    waveCtx.strokeStyle = '#00e5c7';
    waveCtx.lineWidth = 2;
    waveCtx.beginPath();
    const step = w / waveBuf.length;
    waveBuf.forEach((v, i) => {
      const y = h / 2 - (v * h) / 2;
      const x = i * step;
      i === 0 ? waveCtx.moveTo(x, y) : waveCtx.lineTo(x, y);
    });
    waveCtx.stroke();
  }

  // ---- 屏幕镜像帧（二进制）----
  const mirrorCtx = el.mirror.getContext('2d');
  function handleFrame(buf) {
    if (buf.byteLength < 14) return;
    const view = new DataView(buf);
    // 校验魔数 "EBF1"。
    if (view.getUint8(0) !== 0x45 || view.getUint8(1) !== 0x42 ||
        view.getUint8(2) !== 0x46 || view.getUint8(3) !== 0x31) return;
    const width = view.getUint16(4, true);
    const height = view.getUint16(6, true);
    const format = view.getUint8(8); // 1=RGB888 2=RGB565
    const pixels = new Uint8Array(buf, 14);

    const img = mirrorCtx.createImageData(width, height);
    if (format === 1) { // RGB888 → RGBA
      for (let i = 0, j = 0; i < width * height; i++) {
        img.data[j++] = pixels[i * 3];
        img.data[j++] = pixels[i * 3 + 1];
        img.data[j++] = pixels[i * 3 + 2];
        img.data[j++] = 255;
      }
    }
    mirrorCtx.putImageData(img, 0, 0);
    el.mirror.style.display = 'block';
    el.faceFallback.style.display = 'none';
  }

  // ---- 用户交互 → 出站命令 ----
  el.composer.addEventListener('submit', (e) => {
    e.preventDefault();
    const text = el.input.value.trim();
    if (!text) return;
    send(CliType.SendText, { text });
    el.input.value = '';
  });

  document.querySelectorAll('.qa[data-action]').forEach((btn) => {
    btn.addEventListener('click', () => send(CliType.PlayAction, { name: btn.dataset.action, loops: 1 }));
  });

  document.querySelectorAll('.chip[data-emotion]').forEach((btn) => {
    btn.addEventListener('click', () => send(CliType.SetEmotion, { emotion: btn.dataset.emotion }));
  });

  el.interrupt.addEventListener('click', () => send(CliType.Interrupt, { reason: 'user' }));

  el.model.addEventListener('change', () => send(CliType.SelectModel, { id: el.model.value }));

  // 麦克风：点击切换拾音开/关。
  let micOn = false;
  el.mic.addEventListener('click', () => {
    micOn = !micOn;
    el.mic.classList.toggle('active', micOn);
    send(CliType.Mic, { action: micOn ? 'start' : 'stop' });
  });

  // ---- 提示条 ----
  let toastTimer = null;
  function toast(msg) {
    el.toast.textContent = msg;
    el.toast.classList.add('show');
    clearTimeout(toastTimer);
    toastTimer = setTimeout(() => el.toast.classList.remove('show'), 3000);
  }

  // 启动。
  connect();
})();
