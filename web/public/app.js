/*
 * ElectronStudio 前端逻辑（纯原生 JS，无依赖、无构建）。
 *
 * 维护一条到后端的 WebSocket，按 internal/protocol 契约收发消息，驱动三个视图：
 * 对话首页 / 动作编排 / 设置。消息常量与 web/src/protocol.ts 保持一致。
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

  // 6 轴关节名称（顺序与后端 robot.JointNames / 官方 RobotController 完全一致）。
  const JOINT_NAMES = ['左臂横滚', '左臂俯仰', '右臂横滚', '右臂俯仰', '头部俯仰', '身体旋转'];
  const JOINT_COUNT = 6;
  const VOICE_LABEL = { idle: '待命', connecting: '连接中', listening: '聆听中…', thinking: '思考中…', speaking: '回应中…' };

  const $ = (id) => document.getElementById(id);
  const el = {
    conn: $('conn-state'),
    dotUSB: $('dot-usb'), dotASR: $('dot-asr'), dotTTS: $('dot-tts'),
    model: $('model-select'),
    face: $('robot-face'), faceFallback: $('face-fallback'), mirror: $('mirror'),
    voiceState: $('voice-state'), vsText: $('vs-text'), waveform: $('waveform'),
    chat: $('chat-stream'), composer: $('composer'), input: $('composer-input'),
    mic: $('btn-mic'), interrupt: $('btn-interrupt'), toast: $('toast'), camera: $('btn-camera'),
    // 编排页
    actionList: $('action-list'), joints: $('joints'), jogEnable: $('jog-enable'), choreoStop: $('choreo-stop'),
    // 设置页
    modelList: $('model-list'), setASR: $('set-asr'), setTTS: $('set-tts'),
    setUSB: $('set-usb'), setVidPid: $('set-vidpid'), setFPS: $('set-fps'),
  };

  // ======================================================================
  // 视图切换
  // ======================================================================
  document.querySelectorAll('.nav-btn').forEach((btn) => {
    btn.addEventListener('click', () => {
      const view = btn.dataset.view;
      document.querySelectorAll('.nav-btn').forEach((b) => b.classList.toggle('active', b === btn));
      document.querySelectorAll('.view').forEach((v) => v.classList.toggle('active', v.dataset.view === view));
    });
  });

  // ======================================================================
  // WebSocket（带自动重连）
  // ======================================================================
  let ws = null, reconnectTimer = null;

  function connect() {
    const proto = location.protocol === 'https:' ? 'wss' : 'ws';
    ws = new WebSocket(`${proto}://${location.host}/ws`);
    ws.binaryType = 'arraybuffer';
    ws.onopen = () => setConn('已连接', 'ok');
    ws.onclose = () => {
      setConn('已断开 · 重连中', 'off');
      clearTimeout(reconnectTimer);
      reconnectTimer = setTimeout(connect, 1500);
    };
    ws.onerror = () => ws.close();
    ws.onmessage = (e) => {
      if (typeof e.data === 'string') handleEnvelope(e.data);
      else handleFrame(e.data);
    };
  }
  function send(type, payload) {
    if (!ws || ws.readyState !== WebSocket.OPEN) return;
    ws.send(JSON.stringify({ type, ts: Date.now(), payload }));
  }
  function setConn(text, cls) { el.conn.textContent = text; el.conn.className = 'conn-state ' + (cls || ''); }

  // ======================================================================
  // 入站消息分发
  // ======================================================================
  function handleEnvelope(raw) {
    let env; try { env = JSON.parse(raw); } catch { return; }
    const p = env.payload || {};
    switch (env.type) {
      case SrvType.Status: onStatus(p); break;
      case SrvType.VoiceState: setVoice(p.state || 'idle'); break;
      case SrvType.VAD: onVAD(p); break;
      case SrvType.Wake: setVoice('listening'); break;
      case SrvType.Chat: onChat(p); break;
      case SrvType.Emotion: el.face.dataset.emotion = p.emotion || 'neutral'; break;
      case SrvType.Joints: onJoints(p); break;
      case SrvType.Error: toast(p.message || '发生错误'); break;
      default: break;
    }
  }

  function onStatus(s) {
    toggleDot(el.dotUSB, s.robot && s.robot.connected);
    toggleDot(el.dotASR, s.asr && s.asr.running);
    toggleDot(el.dotTTS, s.tts && s.tts.running);
    if (s.llm) {
      renderModelPicker(s.llm);
      renderModelList(s.llm);
    }
    if (s.actions) renderActions(s.actions);
    el.camera.style.display = s.camera ? '' : 'none'; // 配置了摄像头才显示按钮
    renderSettings(s);
  }

  // 摄像头开关：切换屏幕在"表情脸 / 摄像头画面"之间。
  let cameraOn = false;
  el.camera.addEventListener('click', () => {
    cameraOn = !cameraOn;
    el.camera.classList.toggle('active', cameraOn);
    el.camera.textContent = cameraOn ? '🙂 表情' : '📷 摄像头';
    send(CliType.Camera, { enable: cameraOn });
  });
  function toggleDot(node, on) { node.classList.toggle('on', !!on); }

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
  function onVAD(p) { waveBuf.push(p.speaking ? (p.level || 0.5) : 0); waveBuf.shift(); drawWave(); }
  function drawWave() {
    const w = el.waveform.width, h = el.waveform.height;
    waveCtx.clearRect(0, 0, w, h);
    waveCtx.strokeStyle = '#00e5c7'; waveCtx.lineWidth = 2; waveCtx.beginPath();
    const step = w / waveBuf.length;
    waveBuf.forEach((v, i) => {
      const y = h / 2 - (v * h) / 2, x = i * step;
      i === 0 ? waveCtx.moveTo(x, y) : waveCtx.lineTo(x, y);
    });
    waveCtx.stroke();
  }

  // ---- 屏幕镜像帧（二进制）----
  const mirrorCtx = el.mirror.getContext('2d');
  function handleFrame(buf) {
    if (buf.byteLength < 14) return;
    const view = new DataView(buf);
    if (view.getUint8(0) !== 0x45 || view.getUint8(1) !== 0x42 ||
        view.getUint8(2) !== 0x46 || view.getUint8(3) !== 0x31) return;
    const width = view.getUint16(4, true), height = view.getUint16(6, true);
    const format = view.getUint8(8), pixels = new Uint8Array(buf, 14);
    const img = mirrorCtx.createImageData(width, height);
    if (format === 1) {
      for (let i = 0, j = 0; i < width * height; i++) {
        img.data[j++] = pixels[i * 3]; img.data[j++] = pixels[i * 3 + 1];
        img.data[j++] = pixels[i * 3 + 2]; img.data[j++] = 255;
      }
    }
    mirrorCtx.putImageData(img, 0, 0);
    el.mirror.style.display = 'block';
    el.faceFallback.style.display = 'none';
  }

  // ======================================================================
  // 动作编排页
  // ======================================================================
  // 构建 6 轴滑块（仅一次）。
  function buildJoints() {
    for (let i = 0; i < JOINT_COUNT; i++) {
      const row = document.createElement('div');
      row.className = 'joint';
      row.innerHTML =
        `<label>J${i} ${JOINT_NAMES[i] || ''}</label>` +
        `<input type="range" min="-90" max="90" step="1" value="0" data-joint="${i}" />` +
        `<span class="readout"><span class="tgt" id="tgt-${i}">0°</span> / <span class="fb" id="fb-${i}">0°</span></span>`;
      el.joints.appendChild(row);
    }
    // 拖动滑块 → 下发 jog_joint。
    el.joints.querySelectorAll('input[type="range"]').forEach((inp) => {
      inp.addEventListener('input', () => {
        const j = Number(inp.dataset.joint), angle = Number(inp.value);
        $(`tgt-${j}`).textContent = angle + '°';
        send(CliType.JogJoint, { joint: j, angle, enable: el.jogEnable.checked });
      });
    });
  }
  // joints 事件 → 更新反馈读数；跟随设备时同步移动滑块。
  let followActive = false;
  function onJoints(p) {
    if (!Array.isArray(p.angles)) return;
    p.angles.forEach((a, i) => {
      const fb = $(`fb-${i}`);
      if (fb) fb.textContent = Math.round(a) + '°';
      if (followActive) {
        const slider = el.joints.querySelector(`input[data-joint="${i}"]`);
        const tgt = $(`tgt-${i}`);
        if (slider) slider.value = Math.round(a);
        if (tgt) tgt.textContent = Math.round(a) + '°';
      }
    });
  }
  // 渲染动作库按钮（数据驱动自 status.actions），每个带删除。
  function renderActions(actions) {
    el.actionList.innerHTML = '';
    actions.forEach((name) => {
      const wrap = document.createElement('span');
      wrap.className = 'action-item';
      const b = document.createElement('button');
      b.className = 'qa';
      b.textContent = name;
      b.addEventListener('click', () => send(CliType.PlayAction, { name, loops: 1 }));
      const del = document.createElement('button');
      del.className = 'mr-rm';
      del.textContent = '✕';
      del.title = '删除动作';
      del.addEventListener('click', () => { if (confirm(`删除动作「${name}」？`)) send(CliType.DeleteAction, { name }); });
      wrap.appendChild(b);
      wrap.appendChild(del);
      el.actionList.appendChild(wrap);
    });
  }
  el.choreoStop.addEventListener('click', () => send(CliType.Interrupt, { reason: 'choreo-stop' }));

  // 跟随设备 + 示教录制。
  function setupRecord() {
    const follow = $('follow-toggle'), recName = $('rec-name'), recStatus = $('rec-status');
    if (!follow) return;
    follow.addEventListener('change', () => {
      followActive = follow.checked;
      send(CliType.Follow, { enable: follow.checked });
    });
    $('rec-start').addEventListener('click', () => {
      const name = recName.value.trim();
      if (!name) { toast('请填写动作名'); return; }
      send(CliType.RecordStart, { name });
      recStatus.textContent = `录制中：${name}（采帧 0）`;
      recStatus.dataset.count = '0';
    });
    $('rec-frame').addEventListener('click', () => {
      send(CliType.RecordFrame, {});
      const n = (parseInt(recStatus.dataset.count || '0', 10) + 1);
      recStatus.dataset.count = String(n);
      recStatus.textContent = `录制中：${recName.value.trim()}（采帧 ${n}）`;
    });
    $('rec-stop').addEventListener('click', () => {
      send(CliType.RecordStop, {});
      recStatus.textContent = '已保存';
    });
  }

  // ======================================================================
  // 设置页
  // ======================================================================
  function renderModelList(llm) {
    el.modelList.innerHTML = '';
    const list = llm.available || [];
    list.forEach((m) => {
      const active = m.id === llm.active;
      const row = document.createElement('div');
      row.className = 'model-row' + (active ? ' active' : '');
      row.innerHTML =
        `<span class="mr-name">${m.name}</span>` +
        `<span class="mr-prov">${m.provider}</span>` +
        (active ? `<span class="mr-active">● 生效</span>` : '') +
        `<button class="mr-rm" title="删除">✕</button>`;
      // 点击行切换生效模型。
      row.addEventListener('click', () => send(CliType.SelectModel, { id: m.id }));
      // 删除按钮（阻止冒泡，避免触发切换）。
      row.querySelector('.mr-rm').addEventListener('click', (e) => {
        e.stopPropagation();
        if (list.length <= 1) { toast('至少保留一个模型'); return; }
        send(CliType.RemoveModel, { id: m.id });
      });
      el.modelList.appendChild(row);
    });
  }
  // 添加模型表单：按种类显示/隐藏 OpenAI 专属字段，提交发送 add_model。
  function setupAddModelForm() {
    const form = $('add-model');
    if (!form) return;
    const typeSel = $('am-type');
    const toggleOpenAI = () => {
      const isOpenAI = typeSel.value === 'openai';
      document.querySelectorAll('.am-openai').forEach((r) => { r.style.display = isOpenAI ? 'flex' : 'none'; });
    };
    typeSel.addEventListener('change', toggleOpenAI);
    toggleOpenAI();

    form.addEventListener('submit', (e) => {
      e.preventDefault();
      const type = typeSel.value;
      const name = $('am-name').value.trim();
      if (!name) { toast('请填写显示名'); return; }
      const payload = { name, type };
      if (type === 'openai') {
        payload.base_url = $('am-base').value.trim();
        payload.model = $('am-model').value.trim();
        payload.api_key = $('am-key').value;
        if (!payload.base_url || !payload.model) { toast('请填写 Base URL 与模型名'); return; }
      }
      send(CliType.AddModel, payload);
      form.reset();
      toggleOpenAI();
    });
  }

  function renderSettings(s) {
    if (s.asr) el.setASR.textContent = (s.asr.running ? '在线' : '离线') + (s.asr.detail ? ` · ${s.asr.detail}` : '');
    if (s.tts) el.setTTS.textContent = (s.tts.running ? '在线' : '离线') + (s.tts.detail ? ` · ${s.tts.detail}` : '');
    if (s.robot) {
      el.setUSB.textContent = s.robot.connected ? '已连接' : '未连接';
      el.setVidPid.textContent = `0x${(s.robot.vid || 0).toString(16)} / 0x${(s.robot.pid || 0).toString(16)}`;
      el.setFPS.textContent = (s.robot.fps || 0) + ' fps';
    }
  }

  // ---- 顶栏模型下拉 ----
  function renderModelPicker(llm) {
    el.model.innerHTML = '';
    (llm.available || []).forEach((m) => {
      const opt = document.createElement('option');
      opt.value = m.id; opt.textContent = m.name;
      if (m.id === llm.active) opt.selected = true;
      el.model.appendChild(opt);
    });
  }

  // ======================================================================
  // 用户交互 → 出站命令（对话首页）
  // ======================================================================
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
  $('btn-greet').addEventListener('click', () => send(CliType.Greet, {}));
  el.model.addEventListener('change', () => send(CliType.SelectModel, { id: el.model.value }));

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
  buildJoints();
  setupAddModelForm();
  setupRecord();
  connect();
})();
