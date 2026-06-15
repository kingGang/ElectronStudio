/*
 * ElectronStudio 前端逻辑（纯原生 JS，无依赖、无构建）。
 *
 * 维护一条到后端的 WebSocket，按 internal/protocol 契约收发消息，驱动三个视图：
 * 对话首页 / 动作编排 / 设置。消息常量与 web/src/protocol.ts 保持一致。
 */
(() => {
  'use strict';

  // ---- 协议常量（镜像自 internal/protocol，须与 web/src/protocol.ts 一致）----
  const SrvType = {
    Status: 'status', VoiceState: 'voice_state', VAD: 'vad', Wake: 'wake',
    ASR: 'asr', Chat: 'chat', TTS: 'tts', Emotion: 'emotion',
    Joints: 'joints', Error: 'error', Gesture: 'gesture', MusicState: 'music_state',
    ScheduleList: 'schedule_list', Materials: 'materials', Audio: 'audio', Log: 'log',
  };
  const CliType = {
    SendText: 'send_text', Mic: 'mic', Interrupt: 'interrupt',
    PlayAction: 'play_action', SetEmotion: 'set_emotion',
    SelectModel: 'select_model', JogJoint: 'jog_joint',
    AddModel: 'add_model', RemoveModel: 'remove_model',
    Follow: 'follow', RecordStart: 'record_start', RecordFrame: 'record_frame',
    RecordStop: 'record_stop', DeleteAction: 'delete_action',
    Camera: 'camera', Greet: 'greet', Music: 'music',
    ScheduleAdd: 'schedule_add', ScheduleRemove: 'schedule_remove',
    MaterialDelete: 'material_delete', SetIO: 'set_io',
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
      case SrvType.VoiceState: onServerVoice(p.state || 'idle'); break;
      case SrvType.VAD: onVAD(p); break;
      case SrvType.Wake: setVoice('listening'); break;
      case SrvType.Chat: onChat(p); break;
      case SrvType.Emotion: el.face.dataset.emotion = p.emotion || 'neutral'; break;
      case SrvType.Joints: onJoints(p); break;
      case SrvType.Gesture: toast('识别到手势：' + (p.name || '')); break;
      case SrvType.MusicState: onMusicState(p); break;
      case SrvType.ScheduleList: renderSchedule(p.jobs || []); break;
      case SrvType.Materials: renderMaterials(p.materials || []); break;
      case SrvType.Audio: onAudio(p); break;
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
    if (s.io) {
      audioInMode = s.io.audio_in || 'device';
      audioOutMode = s.io.audio_out || 'page';
      el.mic.title = audioInMode === 'page' ? '网页麦克风说话（浏览器识别）' : '切换设备拾音';
      renderIO(s.io);
    }
    if (s.music) {
      musicSource = s.music.source || '';
      const lbl = $('music-source-label');
      if (lbl) lbl.textContent = musicSource === 'qq' ? 'QQ音乐' : (musicSource === 'kuwo' ? '酷我音乐' : musicSource || '—');
    }
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

  let pageVoiceActive = false; // 页面正在播放 TTS 语音（此时由浏览器播放进度决定何时回待命）
  function setVoice(state) {
    el.face.dataset.state = state;
    el.voiceState.dataset.state = state;
    el.vsText.textContent = VOICE_LABEL[state] || state;
  }
  // 服务端语音状态：若页面还在实际出声，服务端提前发来的"待命"先忽略，
  // 等浏览器把语音播完再回待命——保证状态与真实说话同步（实时）。
  function onServerVoice(state) {
    if (pageVoiceActive && state === 'idle') return;
    setVoice(state);
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
    if (p.images && p.images.length) { // 生成图(页面调试镜像)
      node.querySelectorAll('.msg-img').forEach((n) => n.remove());
      p.images.forEach((src) => {
        const im = document.createElement('img');
        im.className = 'msg-img';
        im.src = src; im.alt = '生成图';
        node.appendChild(im);
      });
    }
    if (p.audio) { // 生成的音乐/音频：聊天里给一个带控件的播放器卡片，可重播
      let au = node.querySelector('.msg-audio');
      if (!au) {
        au = document.createElement('audio');
        au.className = 'msg-audio';
        au.controls = true;
        au.autoplay = true; // 尝试自动播放，被拦时用户可点控件
        node.appendChild(au);
      }
      if (au.getAttribute('src') !== p.audio) au.src = p.audio;
    }
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
    // 同步到素材页的实时预览画布（若存在）——点「▶ 预览」即可在素材页看到该情绪。
    const mc = $('mat-mirror');
    if (mc) mc.getContext('2d').putImageData(img, 0, 0);
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

  // ---- I/O 路由设置（设置页下拉，改动即时发 set_io）----
  const IO_FIELDS = ['audio_in', 'audio_out', 'tts_engine', 'image_out'];
  function renderIO(io) {
    IO_FIELDS.forEach((f) => { const sel = $('io-' + f); if (sel && io[f]) sel.value = io[f]; });
  }
  function setupIO() {
    IO_FIELDS.forEach((f) => {
      const sel = $('io-' + f);
      if (!sel) return;
      sel.addEventListener('change', () => {
        const payload = {}; payload[f] = sel.value;
        send(CliType.SetIO, payload);
        toast('已更新：' + f + ' = ' + sel.value);
      });
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

  // 麦克风按钮：audio_in=page 用浏览器内置语音识别（网页麦）；否则切换设备拾音（sidecar）。
  let audioInMode = 'device';
  let audioOutMode = 'page'; // 音频输出路由（来自 status.io.audio_out），决定音乐是否在浏览器播放
  let musicSource = ''; // 当前音源（来自 status.music.source）：qq | kuwo
  let pendingMusicPlay = false; // 刷新恢复时被自动播放策略拦下，待首次手势续播
  let micOn = false;
  const SR = window.SpeechRecognition || window.webkitSpeechRecognition;
  let recog = null, recogOn = false, recogGotFinal = false;

  function ensureRecog() {
    if (recog || !SR) return recog;
    recog = new SR();
    recog.lang = 'zh-CN';
    recog.interimResults = true;
    recog.continuous = false;
    recog.onresult = (e) => {
      let finalText = '';
      for (let i = e.resultIndex; i < e.results.length; i++) {
        const r = e.results[i];
        if (r.isFinal) finalText += r[0].transcript;
        else el.input.value = r[0].transcript; // 中间态显示在输入框
      }
      if (finalText.trim()) {
        recogGotFinal = true;
        el.input.value = '';
        send(CliType.SendText, { text: finalText.trim() }); // 当作一次对话输入
      }
    };
    recog.onend = () => {
      recogOn = false;
      el.mic.classList.remove('active');
      if (!recogGotFinal) setVoice('idle');
    };
    recog.onerror = (ev) => {
      recogOn = false;
      el.mic.classList.remove('active');
      if (ev.error !== 'no-speech' && ev.error !== 'aborted') toast('语音识别出错：' + ev.error);
    };
    return recog;
  }

  el.mic.addEventListener('click', () => {
    if (audioInMode === 'page') {
      if (!SR) { toast('当前浏览器不支持语音识别（建议用 Chrome/Edge）'); return; }
      ensureRecog();
      if (recogOn) { recog.stop(); return; }
      try { recogGotFinal = false; recog.start(); recogOn = true; el.mic.classList.add('active'); setVoice('listening'); }
      catch (_) { /* 已在识别中 */ }
    } else {
      micOn = !micOn;
      el.mic.classList.toggle('active', micOn);
      send(CliType.Mic, { action: micOn ? 'start' : 'stop' });
    }
  });

  // ---- 音频播放（页面调试镜像：后端把合成语音 base64 推来，浏览器播放）----
  let currentAudio = null;
  let musicDucked = false; // 因播放语音回复而临时暂停了音乐，待回复结束续播
  function stopAudio() { if (currentAudio) { try { currentAudio.pause(); } catch (_) {} currentAudio = null; } }
  // 语音回复结束/被打断后：续播音乐 + 让语音状态回到待命（与真实播放同步）。
  function endVoicePlayback() {
    if (musicDucked && musicAudio) { musicAudio.play().catch(() => {}); }
    musicDucked = false;
    if (pageVoiceActive) { pageVoiceActive = false; setVoice('idle'); }
  }
  function onAudio(p) {
    if (!p) return;
    stopAudio();              // 先停上一段（打断/新段都先停）
    if (p.stop) { endVoicePlayback(); return; } // 仅停止（barge-in）→ 续播音乐、回待命
    let src = null;
    if (p.url) {
      src = p.url;            // 较大音频（音乐）走 HTTP URL
    } else if (p.data) {
      const mime = (!p.format || p.format === 'mp3') ? 'mpeg' : p.format; // mp3 的标准 MIME 是 audio/mpeg
      src = 'data:audio/' + mime + ';base64,' + p.data; // 小段语音走 base64
    }
    if (!src) return;
    // 让路：放语音回复前，先暂停正在播放的音乐，回复结束再自动续播。
    if (musicAudio && !musicAudio.paused) { try { musicAudio.pause(); } catch (_) {} musicDucked = true; }
    // 语音状态实时化：开始出声即"说话"，由浏览器播放进度决定何时回"待命"。
    pageVoiceActive = true;
    setVoice('speaking');
    try {
      currentAudio = new Audio(src);
      currentAudio.addEventListener('ended', endVoicePlayback);
      currentAudio.addEventListener('error', endVoicePlayback);
      currentAudio.play().catch(() => { endVoicePlayback(); }); // 播不了也别卡在"说话"/暂停音乐
    } catch (_) { endVoicePlayback(); }
  }

  // ---- 提示条 ----
  let toastTimer = null;
  function toast(msg) {
    el.toast.textContent = msg;
    el.toast.classList.add('show');
    clearTimeout(toastTimer);
    toastTimer = setTimeout(() => el.toast.classList.remove('show'), 3000);
  }

  // ---- 音乐播放控制条（搜歌走 AI 对话框：直接说"放首XXX"）----
  // audio_out=page/both 时由浏览器 <audio> 直接播放事件里的 URL；device 时设备(mpg123)出声、页面只显示状态。
  let musicPlaying = false;
  let musicAudio = null;        // 浏览器播放器（页面输出时）
  let curTrack = { name: '', artist: '' }; // 当前曲目，避免从 DOM 反解
  function sourceLabel() {
    return musicSource === 'qq' ? 'QQ音乐' : (musicSource === 'kuwo' ? '酷我音乐' : '');
  }
  function fmtTime(sec) {
    if (!isFinite(sec) || sec < 0) return '0:00';
    sec = Math.floor(sec);
    const m = Math.floor(sec / 60), s = sec % 60;
    return m + ':' + (s < 10 ? '0' : '') + s;
  }
  function renderMusicLabel() {
    const src = sourceLabel();
    $('music-now').textContent = (musicPlaying ? '' : '⏸ ') + (curTrack.name || '') +
      (curTrack.artist ? ' - ' + curTrack.artist : '') + (src ? '　·　来源：' + src : '');
    $('music-pause').textContent = musicPlaying ? '⏸' : '▶';
  }
  function renderMusicTime() {
    const t = $('music-time'), seek = $('music-seek');
    if (!t) return;
    if (musicAudio && isFinite(musicAudio.duration)) {
      t.textContent = fmtTime(musicAudio.currentTime) + ' / ' + fmtTime(musicAudio.duration);
      if (seek) { seek.max = musicAudio.duration || 0; if (!seek.dragging) seek.value = musicAudio.currentTime || 0; }
    } else {
      t.textContent = audioOutMode === 'device' ? '设备播放中' : '--:-- / --:--';
      if (seek) { seek.max = 0; seek.value = 0; }
    }
  }
  function onMusicState(p) {
    const bar = $('music-bar');
    // 重连恢复事件：仅在本会话尚无播放器(=真刷新/首次)时生效。若浏览器里已有音乐在放
    // （只是 WebSocket 断线重连），忽略恢复，避免打断或让进度跳动/“停了又继续”。
    if (p.restore && musicAudio && musicAudio._srcUrl) return;
    if (p.state === 'stopped' || !p.state) {
      bar.style.display = 'none';
      musicPlaying = false;
      if (musicAudio) { musicAudio.pause(); musicAudio = null; }
      return;
    }
    bar.style.display = 'flex';
    musicPlaying = p.state === 'playing';
    if (p.name) curTrack = { name: p.name, artist: p.artist || '' };
    // 页面输出：用浏览器播放 URL（可读进度/可拖动/可画波形）
    if (p.url && (audioOutMode === 'page' || audioOutMode === 'both')) {
      if (!musicAudio) bindMusicAudio(new Audio());
      if (musicAudio._srcUrl !== p.url) {
        musicAudio._srcUrl = p.url;
        // 经本地代理转发=同源，浏览器 Web Audio 才能读到采样画波形（跨域会拿到全 0）。
        musicAudio.src = '/api/music-proxy?url=' + encodeURIComponent(p.url);
      }
      // 刷新/重连恢复：seek 到后端记录的进度。
      if (p.position > 0) seekWhenReady(musicAudio, p.position);
      if (p.state === 'playing') {
        ensureViz();
        musicAudio.play().then(startViz).catch(() => {
          // 自动播放被拦（刷新后无手势）：标记待续播，点一下页面即恢复。
          pendingMusicPlay = true;
          toast(p.restore ? '点一下页面继续上次的音乐' : '点 ▶ 开始播放');
        });
      } else if (p.state === 'paused') {
        musicAudio.pause();
      }
    }
    renderMusicLabel();
    renderMusicTime();
  }
  // 等元数据就绪后 seek（src 刚设置时 duration 还不可用）。
  function seekWhenReady(a, sec) {
    const doSeek = () => { try { a.currentTime = sec; } catch (_) {} a.removeEventListener('loadedmetadata', doSeek); };
    if (a.readyState >= 1) doSeek(); else a.addEventListener('loadedmetadata', doSeek);
  }
  // 节流上报播放进度/状态给后端（刷新重连可恢复）。
  let lastReport = 0;
  function reportMusic(force) {
    if (!musicAudio) return;
    const now = Date.now();
    if (!force && now - lastReport < 3000) return;
    lastReport = now;
    send(CliType.Music, { action: 'report', position: musicAudio.currentTime || 0, playing: !musicAudio.paused });
  }
  // 绑定浏览器播放器的进度/状态事件。
  function bindMusicAudio(a) {
    musicAudio = a;
    a.crossOrigin = 'anonymous';
    a.addEventListener('timeupdate', () => { renderMusicTime(); reportMusic(false); });
    a.addEventListener('loadedmetadata', renderMusicTime);
    a.addEventListener('play', () => { musicPlaying = true; renderMusicLabel(); startViz(); reportMusic(true); });
    a.addEventListener('pause', () => { musicPlaying = false; renderMusicLabel(); stopViz(); reportMusic(true); });
    // 一首放完自动切下一首（连续播放），由服务端解析下一首 URL 再下发 music_state。
    a.addEventListener('ended', () => { stopViz(); send(CliType.Music, { action: 'next' }); });
  }

  // ---- 音乐实时频谱（Web Audio AnalyserNode → 播放器内居中柱状条）----
  const vizCanvas = $('music-viz'), vizCtx = vizCanvas.getContext('2d');
  const VIZ_BARS = 24;
  let audioCtx = null, vizRAF = null;
  let vizBars = new Array(VIZ_BARS).fill(0); // 平滑后的柱高，避免抖动太硬
  function ensureViz() {
    // 关键：绝不为波形牺牲声音。只有 AudioContext 确实在 running 时才把 <audio> 接入
    // 分析图——否则音频会被导进挂起的 Web Audio 图里导致 play() 成功却无声。
    // 挂起时只尝试 resume，不建 source，音频照常从元素直接出声（暂时无波形）。
    try {
      if (!audioCtx) audioCtx = new (window.AudioContext || window.webkitAudioContext)();
      if (audioCtx.state === 'suspended') { audioCtx.resume(); return; }
      if (audioCtx.state === 'running' && musicAudio && !musicAudio._analyser) {
        const src = audioCtx.createMediaElementSource(musicAudio);
        const an = audioCtx.createAnalyser();
        an.fftSize = 256;
        an.smoothingTimeConstant = 0.8;
        src.connect(an);
        an.connect(audioCtx.destination); // 接入图后必须连到输出，否则没声音
        musicAudio._analyser = an;
      }
    } catch (e) { /* createMediaElementSource 每个元素只能建一次，忽略重复 */ }
  }
  // 全局手势兜底：任何点击/按键都尝试唤醒被浏览器策略挂起的 AudioContext。
  ['pointerdown', 'keydown', 'touchstart'].forEach((ev) =>
    document.addEventListener(ev, () => {
      if (audioCtx && audioCtx.state === 'suspended') audioCtx.resume();
      // 刷新恢复时被自动播放策略拦下的音乐，首次手势即续播。
      if (pendingMusicPlay && musicAudio) {
        pendingMusicPlay = false;
        ensureViz();
        musicAudio.play().then(startViz).catch(() => {});
      }
    }, { passive: true }));
  function startViz() {
    if (vizRAF) return;
    const an = musicAudio && musicAudio._analyser;
    if (!an) return; // 没接上分析图（AudioContext 挂起）→ 不显示波形，但音频照常播放
    $('music-bar').classList.add('playing');
    const data = new Uint8Array(an.frequencyBinCount);
    const loop = () => {
      if (!musicAudio || musicAudio.paused) { vizRAF = null; return; }
      an.getByteFrequencyData(data);
      drawSpectrum(data);
      vizRAF = requestAnimationFrame(loop);
    };
    vizRAF = requestAnimationFrame(loop);
  }
  function stopViz() {
    if (vizRAF) { cancelAnimationFrame(vizRAF); vizRAF = null; }
    $('music-bar').classList.remove('playing');
    vizBars = new Array(VIZ_BARS).fill(0);
  }
  // 居中柱状频谱：低频在中间、向两侧展开，上下镜像，柱顶圆角。
  function drawSpectrum(data) {
    const w = vizCanvas.width, h = vizCanvas.height, mid = h / 2;
    vizCtx.clearRect(0, 0, w, h);
    const usable = Math.floor(data.length * 0.66); // 丢掉几乎为空的高频段
    const per = Math.max(1, Math.floor(usable / VIZ_BARS));
    const gap = 2, bw = (w - gap * (VIZ_BARS - 1)) / VIZ_BARS;
    for (let b = 0; b < VIZ_BARS; b++) {
      let sum = 0;
      for (let k = 0; k < per; k++) sum += data[b * per + k];
      let v = Math.pow((sum / per) / 255, 0.8); // 0..1，做点压缩让小信号更明显
      vizBars[b] = vizBars[b] * 0.6 + v * 0.4;   // 时间平滑
      const bh = Math.max(2, vizBars[b] * h);
      const x = b * (bw + gap), y = mid - bh / 2;
      const g = vizCtx.createLinearGradient(0, y, 0, y + bh);
      g.addColorStop(0, '#26ffe0'); g.addColorStop(1, '#00b8a0');
      vizCtx.fillStyle = g;
      roundBar(vizCtx, x, y, bw, bh, Math.min(bw / 2, 2));
    }
  }
  function roundBar(c, x, y, w, h, r) {
    c.beginPath();
    if (c.roundRect) c.roundRect(x, y, w, h, r);
    else c.rect(x, y, w, h);
    c.fill();
  }
  // 进度条拖动（仅浏览器播放有效）。
  (function () {
    const seek = $('music-seek');
    if (!seek) return;
    seek.addEventListener('input', () => { seek.dragging = true; if (musicAudio) $('music-time').textContent = fmtTime(seek.value) + ' / ' + fmtTime(musicAudio.duration); });
    seek.addEventListener('change', () => { if (musicAudio) musicAudio.currentTime = parseFloat(seek.value) || 0; seek.dragging = false; });
  })();
  // 控制：本地有浏览器播放器时直接控制它；否则把命令发给服务端（设备侧 mpg123）。
  $('music-pause').addEventListener('click', () => {
    if (musicAudio) {
      if (musicAudio.paused) { ensureViz(); musicAudio.play(); } else musicAudio.pause();
    } else {
      send(CliType.Music, { action: musicPlaying ? 'pause' : 'resume' });
    }
  });
  // ---- QQ 音乐扫码登录 ----
  let qqPollTimer = null;
  function stopQQPoll() { if (qqPollTimer) { clearInterval(qqPollTimer); qqPollTimer = null; } }
  $('qq-login-btn') && $('qq-login-btn').addEventListener('click', async () => {
    stopQQPoll();
    const box = $('qq-qr-box'), img = $('qq-qr-img'), st = $('qq-qr-status');
    box.style.display = 'flex';
    st.textContent = '正在生成二维码…';
    // 加时间戳避免缓存，拉取二维码 PNG
    img.src = '/api/qq-login/start?t=' + Date.now();
    img.onload = () => { st.textContent = '请用手机 QQ 扫码并确认'; };
    img.onerror = () => { st.textContent = '二维码获取失败，请重试'; };
    let tries = 0;
    qqPollTimer = setInterval(async () => {
      tries++;
      if (tries > 100) { stopQQPoll(); st.textContent = '二维码超时，请重新点击'; return; }
      try {
        const r = await fetch('/api/qq-login/poll');
        const d = await r.json();
        if (d.state === 'ok') { stopQQPoll(); st.textContent = '✅ ' + (d.message || '登录成功'); toast('QQ 音乐登录成功'); setTimeout(() => { box.style.display = 'none'; }, 2500); }
        else if (d.state === 'scanned') { st.textContent = '已扫码，请在手机上点确认'; }
        else if (d.state === 'expired') { stopQQPoll(); st.textContent = '二维码已失效，请重新点击按钮'; }
        else if (d.state === 'error') { stopQQPoll(); st.textContent = '登录失败：' + (d.message || ''); }
        else { st.textContent = '等待扫码…'; }
      } catch (e) { /* 网络抖动，下次再试 */ }
    }, 2000);
  });

  $('music-next').addEventListener('click', () => { ensureViz(); send(CliType.Music, { action: 'next' }); });
  $('music-stop').addEventListener('click', () => {
    if (musicAudio) { musicAudio.pause(); musicAudio = null; }
    $('music-bar').style.display = 'none';
    musicPlaying = false;
    send(CliType.Music, { action: 'stop' });
  });

  // ---- 提醒 / 定时任务 ----
  function renderSchedule(jobs) {
    const list = $('schedule-list');
    if (!list) return;
    if (!jobs.length) { list.innerHTML = '<span class="muted">无</span>'; return; }
    list.innerHTML = '';
    jobs.forEach((j) => {
      const when = j.at ? new Date(j.at).toLocaleString() : (j.daily ? '每日 ' + j.daily : (j.every ? '每隔 ' + j.every : ''));
      const row = document.createElement('div');
      row.className = 'model-row';
      row.innerHTML = `<span class="mr-name">${j.title || j.text || j.kind}</span>` +
        `<span class="mr-prov">${when}</span><button class="mr-rm">✕</button>`;
      row.querySelector('.mr-rm').addEventListener('click', () => send(CliType.ScheduleRemove, { id: j.id }));
      list.appendChild(row);
    });
  }
  function setupSchedule() {
    const form = $('add-schedule');
    if (!form) return;
    form.addEventListener('submit', (e) => {
      e.preventDefault();
      const title = $('sch-title').value.trim();
      const when = $('sch-when').value;
      const val = $('sch-val').value.trim();
      if (!title || !val) { toast('请填写内容与时间'); return; }
      const payload = { title, kind: 'say', text: title };
      if (when === 'after') {
        const m = parseInt(val, 10);
        if (!m) { toast('分钟数无效'); return; }
        payload.at = new Date(Date.now() + m * 60000).toISOString();
      } else if (when === 'daily') {
        payload.daily = val;
      } else {
        payload.every = val;
      }
      send(CliType.ScheduleAdd, payload);
      form.reset();
    });
  }

  // ======================================================================
  // 屏幕表情素材（上传 GIF/图片 → 机器人脸；列表/删除走 WS，上传/缩略图走 HTTP）
  // ======================================================================
  let matVersion = 0; // 每次列表更新自增，用于缩略图 URL 去缓存
  function renderMaterials(materials) {
    const grid = $('mat-grid'), count = $('mat-count');
    if (count) count.textContent = materials.length ? `${materials.length} 个情绪` : '';
    if (!grid) return;
    matVersion++;
    if (!materials.length) { grid.innerHTML = '<span class="muted">暂无素材，上传一段 GIF 试试</span>'; return; }
    grid.innerHTML = '';
    materials.forEach((m) => {
      const card = document.createElement('div');
      card.className = 'mat-card';
      const kind = { gif: 'GIF', frames: '帧序列', atlas: '图集' }[m.kind] || m.kind || '';
      card.innerHTML =
        `<img class="mat-thumb" alt="${m.name}" src="/api/material-thumb?name=${encodeURIComponent(m.name)}&v=${matVersion}" />` +
        `<div class="mat-meta"><b class="mat-name">${m.name}</b>` +
        `<span class="mat-sub">${m.frames} 帧 · ${m.fps}fps · ${kind}</span></div>` +
        `<div class="mat-ops">` +
        `<button class="qa mat-preview">▶ 预览</button>` +
        `<button class="mr-rm" title="删除素材">✕</button>` +
        `</div>`;
      card.querySelector('.mat-preview').addEventListener('click', () => {
        send(CliType.SetEmotion, { emotion: m.name, preview: true }); // preview：只切屏，不联动动作
        toast(`预览「${m.name}」`);
      });
      card.querySelector('.mr-rm').addEventListener('click', () => {
        if (confirm(`删除素材「${m.name}」？删除后该情绪回退到程序动画脸。`)) {
          send(CliType.MaterialDelete, { name: m.name });
        }
      });
      grid.appendChild(card);
    });
  }

  // 视频在浏览器内抽帧：用 <video>（浏览器自带任意编解码器解码）+ <canvas> 等比缩放居中裁剪到
  // 240×240，抽成 PNG 帧后上传——服务器纯 Go 收帧，无需 ffmpeg，支持的格式 = 浏览器能播放的一切。
  const VIDEO_RE = /\.(mp4|webm|mov|mkv|avi|m4v)$/i;
  function isVideoFile(file) { return /^video\//.test(file.type) || VIDEO_RE.test(file.name); }

  function seekTo(video, t) {
    return new Promise((resolve) => {
      let done = false;
      const finish = () => { if (done) return; done = true; video.removeEventListener('seeked', finish); resolve(); };
      video.addEventListener('seeked', finish);
      try { video.currentTime = t; } catch { finish(); }
      setTimeout(finish, 3000); // 兜底：个别浏览器/编码不触发 seeked 时不卡死
    });
  }

  async function extractVideoFrames(file, size = 240, fps = 15, maxFrames = 300) {
    const url = URL.createObjectURL(file);
    const video = document.createElement('video');
    video.muted = true; video.playsInline = true; video.preload = 'auto'; video.src = url;
    try {
      await new Promise((res, rej) => {
        video.onloadedmetadata = () => res();
        video.onerror = () => rej(new Error('浏览器无法解码该视频'));
      });
      const dur = video.duration;
      if (!isFinite(dur) || dur <= 0) throw new Error('视频时长无效');
      const n = Math.max(1, Math.min(maxFrames, Math.round(dur * fps)));
      const canvas = document.createElement('canvas');
      canvas.width = size; canvas.height = size;
      const ctx = canvas.getContext('2d');
      const frames = [];
      for (let i = 0; i < n; i++) {
        const t = Math.min(dur - 0.001, (i + 0.5) * dur / n);
        await seekTo(video, t);
        const vw = video.videoWidth || size, vh = video.videoHeight || size;
        const scale = Math.max(size / vw, size / vh); // cover：放大填满后居中裁剪
        const dw = vw * scale, dh = vh * scale;
        ctx.clearRect(0, 0, size, size);
        ctx.drawImage(video, (size - dw) / 2, (size - dh) / 2, dw, dh);
        const blob = await new Promise((r) => canvas.toBlob(r, 'image/png'));
        if (blob) frames.push(blob);
      }
      return { frames, fps };
    } finally {
      URL.revokeObjectURL(url);
    }
  }

  async function uploadVideo(name, file, submit) {
    submit.textContent = '抽帧中…';
    const { frames, fps } = await extractVideoFrames(file);
    if (!frames.length) throw new Error('未能从视频抽取到帧');
    submit.textContent = `上传 ${frames.length} 帧…`;
    const fd = new FormData();
    fd.append('name', name);
    fd.append('fps', String(fps));
    frames.forEach((b, i) => fd.append('frame', b, String(i + 1).padStart(4, '0') + '.png'));
    const res = await fetch('/api/material-frames', { method: 'POST', body: fd });
    if (!res.ok) throw new Error((await res.text()).trim());
  }

  function setupMaterials() {
    const form = $('mat-upload');
    if (!form) return;
    const nameInp = $('mat-name'), fileInp = $('mat-file'), submit = $('mat-submit');
    // 停止预览：回到默认程序脸（neutral 一般无素材，即回退到眨眼/口型的程序动画脸）。
    const stopBtn = $('mat-stop');
    if (stopBtn) stopBtn.addEventListener('click', () => {
      send(CliType.SetEmotion, { emotion: 'neutral', preview: true });
      toast('已停止预览');
    });
    form.addEventListener('submit', async (e) => {
      e.preventDefault();
      const name = nameInp.value.trim().toLowerCase();
      const file = fileInp.files && fileInp.files[0];
      if (!name) { toast('请填写情绪名'); return; }
      if (!/^[\p{L}\p{N}_-]{1,24}$/u.test(name)) { toast('情绪名支持中文/字母/数字/-/_（≤24 字）'); return; }
      if (!file) { toast('请选择 GIF / 视频 / 图片文件'); return; }

      submit.disabled = true;
      submit.textContent = '上传中…';
      try {
        if (isVideoFile(file)) {
          await uploadVideo(name, file, submit); // 浏览器抽帧 → /api/material-frames（无 ffmpeg）
          toast(`已上传「${name}」`);
          form.reset();
        } else {
          const fd = new FormData();
          fd.append('name', name);
          fd.append('file', file);
          const res = await fetch('/api/materials', { method: 'POST', body: fd });
          if (res.ok) { toast(`已上传「${name}」`); form.reset(); }
          else { toast('上传失败：' + (await res.text()).trim()); }
        }
      } catch (err) {
        toast('上传失败：' + (err.message || err));
      } finally {
        submit.disabled = false;
        submit.textContent = '⬆ 上传';
      }
      // 列表由后端 materials 事件刷新，无需手动拉取。
    });
  }

  // 启动。
  buildJoints();
  setupAddModelForm();
  setupSchedule();
  setupRecord();
  setupMaterials();
  setupIO();
  connect();
})();
