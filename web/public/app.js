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
    SelectModel: 'select_model', JogJoint: 'jog_joint', Reenable: 'reenable',
    AddModel: 'add_model', RemoveModel: 'remove_model',
    Follow: 'follow', RecordStart: 'record_start', RecordFrame: 'record_frame',
    RecordStop: 'record_stop', DeleteAction: 'delete_action',
    Camera: 'camera', Greet: 'greet', Music: 'music', Party: 'party',
    ScheduleAdd: 'schedule_add', ScheduleRemove: 'schedule_remove',
    MaterialDelete: 'material_delete', SetIO: 'set_io', SetDevice: 'set_device', SetVolume: 'set_volume',
    SetRealtime: 'set_realtime', RebootDevice: 'reboot_device',
  };

  // 6 轴关节名称（顺序与后端 robot.JointNames 一致，即官方下发给固件的线上顺序：头在 0 号）。
  const JOINT_NAMES = ['头部俯仰', '左臂横滚', '左臂俯仰', '右臂横滚', '右臂俯仰', '身体旋转'];
  const JOINT_COUNT = 6;
  // 各关节角度限制(度)，与后端 robot.JointLimits 同步：头-15~15 / 横滚0~30 / 俯仰-20~180 / 身体-90~90。
  // 两臂俯仰上限 150：真机实测 167 顶到机械限位堵转（会把主控 I²C 卡死）、160 手会撞到头。
  const JOINT_LIMITS = [[-15, 15], [0, 30], [-20, 150], [0, 30], [-20, 150], [-90, 90]];
  const clampJoint = (i, a) => Math.max(JOINT_LIMITS[i][0], Math.min(JOINT_LIMITS[i][1], a));
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
    jogReenable: $('jog-reenable'), rebootDevice: $('reboot-device'),
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
      if (view === 'model3d') init3D(); // 首次打开才加载 11MB 模型
    });
  });

  let wasRecovering = false, wasWedged = false; // 跟踪自愈中/需断电两态，各只在转入时弹一次提示

  // ---- 3D 模型（three.js + GLTFLoader，懒加载）----
  let model3dReady = false;
  let model3dJoints = null;       // {armRollLeft, armPitchLeft, ...} 关节节点
  let lastJointAngles = null;     // 最近一次 6 轴角度，用于模型加载后补帧
  let m3dScene = null, m3dCamera = null, m3dRenderer = null, m3dControls = null, m3dRoot = null;
  const MODEL_URLS = { elite: 'models/electronbot-elite.glb', classic: 'models/electronbot-classic.glb' };
  let currentModel = 'elite'; // 默认精英版外壳；可切回经典版
  let m3dScreenTex = null;    // 头部屏幕贴图（取设备屏实时镜像画面）
  // 屏幕圆面位置/尺寸——由打印件几何算出的正面圆开口圆心与半径（非手调）：
  //   精英版取屏幕支架 head_adapter 正面圆心(-0.9,25.4) z=25.8 半径≈17；
  //   经典版取脸部面板 Head3 正面圆心(0.7,43.2) z=27.1 半径≈16。z 内收 1mm 嵌进开口。
  const SCREEN_CFG = {
    elite:   { pos: [-0.9, 25.4, 24.8], size: [34, 34] },
    classic: { pos: [0, 24.1, 27.3], size: [30, 30] },
  };
  const D2R = Math.PI / 180;
  // 直接拖关节：节点名 → {x:横向拖控制的关节下标, y:纵向拖控制的关节下标}。
  // 手臂(无论抓到 roll/pitch 节点)：横向拖=横滚(往外张)，纵向拖=俯仰(前后)。
  // xs=横向拖的符号，让手臂"往你拖的方向"动（左右臂镜像，符号相反）。
  const DRAG_MAP = {
    armRollLeft:  { x: 1, y: 2, xs: 1 },  armPitchLeft:  { x: 1, y: 2, xs: 1 },
    armRollRight: { x: 3, y: 4, xs: -1 }, armPitchRight: { x: 3, y: 4, xs: -1 },
    head: { y: 0 },          // 抬/低头
    body: { x: 5, xs: 1 },   // 左右转身
  };
  let m3dDragging = false;   // 正在拖关节：期间忽略服务端回传，避免抖动
  // 显示用的平滑角度（度）。舵机反馈是【带噪声的模拟量】：机器人完全静止时，电位器/ADC 读数
  // 仍会在 ±0.5° 上下跳（真机实测：头部俯仰 1.13→0.82→0.50→0.19→0.98），每秒广播 10 次。
  // 把这种原始值直接灌进模型的旋转角，模型就会一直微微发抖——那不是机器人在抖，是噪声在抖。
  // 这里只对【显示】做处理，控制链路照旧用原始值。
  let m3dSmooth = null;
  const JITTER_DEADBAND = 0.4; // 度：小于这个幅度的变化视为噪声，直接忽略（静止时彻底不动）
  const JITTER_ALPHA = 0.35;   // 指数平滑系数：越小越稳、越大越跟手。真机运动时肉眼跟不出延迟
  // 按 ElectronBot 官方 RobotController 的轴/符号驱动 6 关节。
  // angles 顺序 = robot.JointNames: [头部俯仰,左臂横滚,左臂俯仰,右臂横滚,右臂俯仰,身体旋转]
  function drive3D(angles) {
    lastJointAngles = angles;
    const j = model3dJoints;
    if (!j || !j.body) return;
    // 死区 + 指数平滑：静止时完全不动，真动起来又能跟上（幅度远大于死区，一两帧就收敛）。
    if (!m3dSmooth || m3dSmooth.length !== angles.length) {
      m3dSmooth = angles.slice(); // 首帧直接采用，避免从 0 慢慢爬过去
    } else {
      for (let i = 0; i < angles.length; i++) {
        const d = angles[i] - m3dSmooth[i];
        if (Math.abs(d) < JITTER_DEADBAND) continue;         // 噪声：忽略
        m3dSmooth[i] += d * JITTER_ALPHA;                    // 真运动：平滑跟随
      }
    }
    const a = m3dSmooth;
    if (j.head) j.head.rotation.x = a[0] * D2R;
    if (j.armRollLeft) j.armRollLeft.rotation.z = a[1] * D2R;
    if (j.armPitchLeft) j.armPitchLeft.rotation.x = -a[2] * D2R;  // 俯仰：手性相反，取负让正角度朝前
    if (j.armRollRight) j.armRollRight.rotation.z = -a[3] * D2R;
    if (j.armPitchRight) j.armPitchRight.rotation.x = -a[4] * D2R;
    if (j.body) j.body.rotation.y = a[5] * D2R;
  }
  // ARM_DONOR：手臂从哪个模型取。精英版自带的是短粗款，换成经典版的细长手臂。
  // 两个 glb 共用同一套骨架(armRoll*/armPitch*)和同一世界坐标——身体/头的包围盒逐位相同，
  // 手臂枢轴也烘焙在网格顶点里，所以直接把经典版 armPitch* 下的网格搬过来即可，无需任何变换。
  const ARM_DONOR = { elite: 'classic' };
  // graftArms 用 donorRoot 的手臂网格替换 root 的：只换 armPitch* 底下的网格，骨架节点不动
  // （关节仍由 drive3D 驱动 root 自己的 armRoll*/armPitch*）。换上来的手臂沿用【本模型自带手臂的
  // 材质】，这样颜色/质感和外壳其余部分一致，不会一只手臂突兀地是另一套配色。
  function graftArms(root, donorRoot) {
    for (const joint of ['armPitchLeft', 'armPitchRight']) {
      const dst = root.getObjectByName(joint), src = donorRoot.getObjectByName(joint);
      if (!dst || !src) continue;
      let mat = null;
      dst.traverse((o) => { if (!mat && o.isMesh && o.material) mat = o.material; }); // 记下原材质
      for (const mesh of [...dst.children]) dst.remove(mesh);  // 摘掉自带手臂
      for (const mesh of [...src.children]) dst.add(mesh);     // 挂上捐赠者的手臂（自动改父）
      if (mat) dst.traverse((o) => { if (o.isMesh) o.material = mat; }); // 套回本模型的配色
    }
  }
  // 加载（或切换）一个模型到已有场景；移除旧模型，重新抓关节、归位相机。
  function loadModel3D(url) {
    const st = $('model3d-status');
    if (m3dRoot) { m3dScene.remove(m3dRoot); m3dRoot = null; window.electronbotModel = null; }
    model3dJoints = null;
    st.textContent = '加载模型中…';
    const donor = ARM_DONOR[currentModel];
    const withArms = (gltf, next) => {          // 需要换手臂就先把捐赠模型也拉下来
      if (!donor) return next(gltf);
      new THREE.GLTFLoader().load(MODEL_URLS[donor], (d) => {
        graftArms(gltf.scene, d.scene);
        next(gltf);
      }, null, () => next(gltf));               // 捐赠模型加载失败：退回自带手臂，不影响主模型
    };
    new THREE.GLTFLoader().load(url, (gltf) => withArms(gltf, (gltf) => {
      const root = gltf.scene;
      model3dJoints = {
        armRollLeft: root.getObjectByName('armRollLeft'),
        armPitchLeft: root.getObjectByName('armPitchLeft'),
        armRollRight: root.getObjectByName('armRollRight'),
        armPitchRight: root.getObjectByName('armPitchRight'),
        head: root.getObjectByName('head'),
        body: root.getObjectByName('body'),
      };
      const box = new THREE.Box3().setFromObject(root);
      const size = box.getSize(new THREE.Vector3()), center = box.getCenter(new THREE.Vector3());
      root.position.sub(center);
      const maxDim = Math.max(size.x, size.y, size.z) || 1;
      m3dCamera.position.set(0, maxDim * 0.25, maxDim * 1.9);
      m3dControls.target.set(0, 0, 0); m3dControls.update();
      m3dScene.add(root);
      m3dRoot = root; window.electronbotModel = root;
      addScreen(model3dJoints.head, SCREEN_CFG[currentModel]); // 头部贴上实时屏幕画面
      if (lastJointAngles) drive3D(lastJointAngles);
      st.textContent = '拖动旋转 / 滚轮缩放；动作/关节会驱动模型';
    }), (e) => { if (e.total) st.textContent = '加载中 ' + Math.round((e.loaded / e.total) * 100) + '%'; },
      (err) => { st.textContent = '模型加载失败'; console.error(err); });
  }
  // 在头部加一块屏幕平面，贴上设备屏的实时镜像画面（el.mirror canvas）。
  function addScreen(head, cfg) {
    if (!head || !cfg) return;
    if (!m3dScreenTex) {
      m3dScreenTex = new THREE.CanvasTexture(el.mirror);
      if ('SRGBColorSpace' in THREE) m3dScreenTex.colorSpace = THREE.SRGBColorSpace;
    }
    // 圆形屏幕：与头部圆形开口贴合（半径取尺寸一半），不受光照像真屏幕。
    const r = Math.min(cfg.size[0], cfg.size[1]) / 2;
    const plane = new THREE.Mesh(
      new THREE.CircleGeometry(r, 48),
      new THREE.MeshBasicMaterial({ map: m3dScreenTex, side: THREE.DoubleSide })
    );
    plane.position.set(cfg.pos[0], cfg.pos[1], cfg.pos[2]);
    if (cfg.rot) plane.rotation.set(cfg.rot[0] || 0, cfg.rot[1] || 0, cfg.rot[2] || 0);
    plane.name = '__screen';
    head.add(plane);
  }
  function init3D() {
    if (model3dReady || typeof THREE === 'undefined') return;
    model3dReady = true;
    const stage = $('model3d-stage');
    const W = () => stage.clientWidth || 600, H = () => stage.clientHeight || 400;
    const scene = new THREE.Scene(); m3dScene = scene;
    scene.background = new THREE.Color(0x0c1118);
    const camera = new THREE.PerspectiveCamera(45, W() / H(), 0.1, 5000); m3dCamera = camera;
    const renderer = new THREE.WebGLRenderer({ antialias: true }); m3dRenderer = renderer;
    renderer.setPixelRatio(window.devicePixelRatio || 1);
    renderer.setSize(W(), H());
    stage.appendChild(renderer.domElement);
    // 灯光
    scene.add(new THREE.AmbientLight(0xffffff, 0.7));
    const dir = new THREE.DirectionalLight(0xffffff, 0.9); dir.position.set(1, 2, 2); scene.add(dir);
    const dir2 = new THREE.DirectionalLight(0x88aaff, 0.4); dir2.position.set(-2, 1, -1); scene.add(dir2);
    const controls = new THREE.OrbitControls(camera, renderer.domElement); m3dControls = controls;
    controls.enableDamping = true; controls.autoRotate = false; // 不自动旋转，只在用户拖动时转视角
    loadModel3D(MODEL_URLS[currentModel]);
    function loop() {
      requestAnimationFrame(loop);
      controls.update();
      if (m3dScreenTex) m3dScreenTex.needsUpdate = true; // 屏幕画面实时刷新
      renderer.render(scene, camera);
    }
    loop();
    window.addEventListener('resize', () => {
      if (!stage.clientWidth) return;
      camera.aspect = W() / H(); camera.updateProjectionMatrix(); renderer.setSize(W(), H());
    });
    build3DControls();

    // ---- 直接抓关节拖动：点到胳膊/头/身体拖动改该关节；点空白处则旋转视角 ----
    const ray = new THREE.Raycaster();
    let dragMap = null, dragOX = 0, dragOY = 0, dragBX = 0, dragBY = 0, lastJog = 0;
    function pickJoint(ev) {
      const root = window.electronbotModel; if (!root) return null;
      const r = renderer.domElement.getBoundingClientRect();
      const ndc = new THREE.Vector2(((ev.clientX - r.left) / r.width) * 2 - 1, -((ev.clientY - r.top) / r.height) * 2 + 1);
      ray.setFromCamera(ndc, camera);
      const hits = ray.intersectObject(root, true);
      if (!hits.length) return null;
      let o = hits[0].object;
      while (o) { if (DRAG_MAP[o.name]) return o.name; o = o.parent; }
      return null;
    }
    function setJointAngle(i, ang) {
      ang = clampJoint(i, ang);
      const a = (lastJointAngles ? lastJointAngles.slice() : [0, 0, 0, 0, 0, 0]); a[i] = ang; drive3D(a);
      const inp = document.querySelector(`#model3d-controls input[data-m3d="${i}"]`); if (inp) inp.value = Math.round(ang);
      const val = $(`m3d-val-${i}`); if (val) val.textContent = Math.round(ang) + '°';
      return ang;
    }
    function jogNow(i) {
      const now = Date.now();
      if (now - lastJog < 80) return; lastJog = now;
      send(CliType.JogJoint, { joint: i, angle: Math.round((lastJointAngles || [])[i] || 0), enable: true });
    }
    renderer.domElement.addEventListener('pointerdown', (ev) => {
      const jn = pickJoint(ev);
      if (!jn) return; // 点到空白/底座 → 交给 OrbitControls 转视角
      dragMap = DRAG_MAP[jn]; dragOX = ev.clientX; dragOY = ev.clientY;
      const cur = lastJointAngles || [0, 0, 0, 0, 0, 0];
      dragBX = dragMap.x !== undefined ? cur[dragMap.x] : 0;
      dragBY = dragMap.y !== undefined ? cur[dragMap.y] : 0;
      m3dDragging = true;
      controls.enabled = false; // 拖关节时禁掉视角旋转
      renderer.domElement.style.cursor = 'grabbing';
    });
    window.addEventListener('pointermove', (ev) => {
      if (!dragMap) return;
      if (dragMap.x !== undefined) { setJointAngle(dragMap.x, dragBX + (ev.clientX - dragOX) * 0.6 * (dragMap.xs || 1)); jogNow(dragMap.x); }
      if (dragMap.y !== undefined) { setJointAngle(dragMap.y, dragBY + (ev.clientY - dragOY) * 0.6); jogNow(dragMap.y); }
    });
    window.addEventListener('pointerup', () => {
      m3dDragging = false;
      if (dragMap) {
        if (dragMap.x !== undefined) send(CliType.JogJoint, { joint: dragMap.x, angle: Math.round((lastJointAngles || [])[dragMap.x] || 0), enable: true });
        if (dragMap.y !== undefined) send(CliType.JogJoint, { joint: dragMap.y, angle: Math.round((lastJointAngles || [])[dragMap.y] || 0), enable: true });
      }
      dragMap = null; controls.enabled = true; renderer.domElement.style.cursor = '';
    });
  }
  // 3D 视图内的关节滑块：拖动即时驱动模型 + 下发 jog_joint 给设备。
  function build3DControls() {
    const box = $('model3d-controls');
    if (!box || box.dataset.built) return;
    box.dataset.built = '1';
    // 外壳切换
    const msel = $('model3d-select');
    if (msel) { msel.value = currentModel; msel.addEventListener('change', () => { currentModel = msel.value; loadModel3D(MODEL_URLS[currentModel]); }); }
    for (let i = 0; i < JOINT_COUNT; i++) {
      const row = document.createElement('div');
      row.className = 'm3d-joint';
      row.innerHTML = `<label>${JOINT_NAMES[i] || ('J' + i)}</label>` +
        `<input type="range" min="${JOINT_LIMITS[i][0]}" max="${JOINT_LIMITS[i][1]}" step="1" value="0" data-m3d="${i}" />` +
        `<span class="m3d-val" id="m3d-val-${i}">0°</span>`;
      box.appendChild(row);
    }
    box.querySelectorAll('input[type="range"]').forEach((inp) => {
      inp.addEventListener('input', () => {
        const j = Number(inp.dataset.m3d), angle = Number(inp.value);
        $(`m3d-val-${j}`).textContent = angle + '°';
        const a = (lastJointAngles ? lastJointAngles.slice() : [0, 0, 0, 0, 0, 0]);
        a[j] = angle; drive3D(a);                       // 本地即时驱动
        send(CliType.JogJoint, { joint: j, angle, enable: true }); // 同步设备
      });
    });
    const rst = $('model3d-reset');
    if (rst) rst.addEventListener('click', (e) => {
      e.preventDefault();
      box.querySelectorAll('input[type="range"]').forEach((inp) => { inp.value = 0; });
      box.querySelectorAll('.m3d-val').forEach((s) => { s.textContent = '0°'; });
      drive3D([0, 0, 0, 0, 0, 0]);
      for (let j = 0; j < JOINT_COUNT; j++) send(CliType.JogJoint, { joint: j, angle: 0, enable: true });
    });
  }

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
    if (!ws || ws.readyState !== WebSocket.OPEN) {
      // 连接断开时别静默丢命令——否则顶栏下拉显示了新选择、服务端却没收到，
      // 看起来就是「顶栏和设置页不同步」。给出提示，待重连后状态会自动纠回真实值。
      toast('连接已断开，正在重连…请稍候重试');
      return false;
    }
    ws.send(JSON.stringify({ type, ts: Date.now(), payload }));
    return true;
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
    const stuck = !!(s.robot && s.robot.stuck);
    const recovering = !!(s.robot && s.robot.recovering); // 卡死后正在自动软复位(免拔电源)自救中
    const wedged = stuck && !recovering;                  // 自动复位无效/不可用 → 才真的需要手动断电
    // USB 灯反映“在同步”而非仅“已连接”：卡死/自愈时(已连接但无就绪包)灯灭，避免误以为正常。
    toggleDot(el.dotUSB, s.robot && s.robot.connected && !stuck);
    if (el.dotUSB) {
      // 顶栏灯旁标签直接显示连接速度(USB 2.0/3.0)，未连接显示 "USB"。
      if (el.dotUSB.lastChild) el.dotUSB.lastChild.textContent = (s.robot && s.robot.connected && s.robot.speed) ? s.robot.speed : 'USB';
      el.dotUSB.title = recovering ? '检测到卡死，正在自动软复位(免拔电源)…稍候会自动重连'
        : wedged ? '卡死：自动软复位无效，请彻底断电(拔线≥15秒放净电容)再插，或点「复位电子」'
        : (s.robot && s.robot.connected ? ('已连接同步中' + (s.robot.speed ? ' · ' + s.robot.speed : '')) : '未连接');
    }
    // 进入自愈弹一次；升级到"真需断电"再弹一次（两态各只提示一次，避免刷屏）。
    if (recovering && !wasRecovering) toast('检测到卡死，正在自动软复位(免拔电源)，稍候自动重连…');
    if (wedged && !wasWedged) toast('⚠ 自动软复位无效，请彻底断电(拔线≥15秒)再插复位，或点「复位电子」');
    wasRecovering = recovering; wasWedged = wedged;
    toggleDot(el.dotASR, s.asr && s.asr.running);
    toggleDot(el.dotTTS, s.tts && s.tts.running);
    if (s.llm) {
      renderModelPicker(s.llm);
      renderModelList(s.llm);
    }
    if (s.actions) renderActions(s.actions);
    el.camera.style.display = s.camera ? '' : 'none'; // 配置了摄像头才显示按钮
    if (typeof s.camera_on === 'boolean') setCameraBtn(s.camera_on); // 开关状态以后端为准
    if (s.io) {
      audioInMode = s.io.audio_in || 'device';
      audioOutMode = s.io.audio_out || 'page';
      el.mic.title = audioInMode === 'page' ? '网页麦克风说话（浏览器识别）'
        : audioInMode === 'network' ? '网页录音（网络 ASR 识别）：点开始、再点结束'
        : '切换设备拾音';
      renderIO(s.io);
      updateVoiceVisibility(s.io.tts_engine);
      updateSceneRadio(s.io);
    }
    if (s.realtime) renderRealtime(s.realtime);
    if (s.music) {
      musicSource = s.music.source || '';
      const lbl = $('music-source-label');
      if (lbl) lbl.textContent = musicSource === 'qq' ? 'QQ音乐' : (musicSource === 'kuwo' ? '酷我音乐' : musicSource || '—');
      const qstate = $('qq-login-state');
      if (qstate) {
        if (musicSource === 'qq') {
          qstate.textContent = s.music.logged_in ? '✅ 已登录' : '未登录（点下方扫码）';
          qstate.style.color = s.music.logged_in ? 'var(--ok)' : 'var(--busy)';
        } else { qstate.textContent = '—'; qstate.style.color = ''; }
      }
    }
    // 设备角色/音色：仅在用户未正在编辑时回填，避免打断输入。
    const pa = $('dev-persona');
    if (pa && document.activeElement !== pa) pa.value = s.persona || '';
    const psrc = $('dev-persona-source');
    if (psrc && document.activeElement !== psrc) psrc.value = s.persona_source || 'local';
    const prow = $('dev-persona-row');
    if (prow) prow.style.display = (s.persona_source === 'model') ? 'none' : '';
    devVoice = s.voice || '';
    reconcileVoiceSelect();
    renderSettings(s);
  }

  // 摄像头开关：切换屏幕在"表情脸 / 摄像头画面"之间。开/关状态以后端 status.camera_on 为准，
  // 避免本地状态与后端不同步(如摄像头被其它途径开关、或重连后)导致点了没反应。
  let cameraOn = false;
  function setCameraBtn(on) {
    cameraOn = !!on;
    el.camera.classList.toggle('active', cameraOn);
    el.camera.textContent = cameraOn ? '🙂 表情' : '📷 摄像头';
  }
  el.camera.addEventListener('click', () => {
    const next = !cameraOn;
    setCameraBtn(next);                    // 立即反馈，随后由 status.camera_on 回来校正
    send(CliType.Camera, { enable: next });
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
        `<input type="range" min="${JOINT_LIMITS[i][0]}" max="${JOINT_LIMITS[i][1]}" step="1" value="0" data-joint="${i}" />` +
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
    if (!m3dDragging) drive3D(p.angles); // 驱动 3D 模型；拖动期间不被回传覆盖(防抖)
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
  // 重新使能舵机：过载/堵转保护锁存后，舵机会「能报位置但电机不转」，只有 enable 0→1 跳变能解锁。
  el.jogReenable.addEventListener('click', () => { send(CliType.Reenable, {}); toast('已重新给舵机上扭矩'); });
  // 复位电子：固件卡死(屏幕/关节不动、连着却不同步)时，往设备串口发软复位指令——免拔电源，设备会重启并自动重连。
  if (el.rebootDevice) el.rebootDevice.addEventListener('click', () => { send(CliType.RebootDevice, {}); toast('已发送复位指令，设备将重启并自动重连…'); });

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
  // 「使用场景」预设：一键把输入/输出收成两套常用组合（避免同机两路输出回音）。
  const SCENES = {
    web: { audio_in: 'page', audio_out: 'page' },        // 单机网页：全走浏览器
    device: { audio_in: 'device', audio_out: 'device' }, // 机器人/树莓派：全走设备 sidecar
  };
  function renderIO(io) {
    IO_FIELDS.forEach((f) => { const sel = $('io-' + f); if (sel && io[f]) sel.value = io[f]; });
    const vol = $('io-device_volume'), volv = $('io-device_volume-val');
    if (vol && typeof io.device_volume === 'number' && io.device_volume > 0) {
      vol.value = io.device_volume; if (volv) volv.textContent = io.device_volume;
    }
    const servo = $('io-servo_enable');
    if (servo && typeof io.servo_enable === 'boolean') {
      servo.checked = io.servo_enable;
      renderServoNote(io.servo_enable);
    }
  }
  // 舵机总开关的说明文案：关时提示可手动摆姿，开时提示已上扭矩。
  function renderServoNote(on) {
    const n = $('io-servo_enable-note');
    if (n) n.textContent = on ? '开：舵机已上扭矩，可执行动作' : '关：不上扭矩，可手动摆姿';
  }
  // 音色区可见性：仅 MiniMax/OpenAI 引擎可选音色；小智/本地引擎给一句说明，不再空着像坏了。
  function curEngine() { const e = $('io-tts_engine'); return e ? e.value : 'minimax'; }
  function updateVoiceVisibility(engine) {
    engine = engine || curEngine();
    const usesVoice = engine === 'minimax' || engine === 'openai';
    const row = $('dev-voice-row'), custom = $('dev-voice-custom-row'), note = $('dev-voice-note');
    const sel = $('dev-voice-select');
    if (row) row.style.display = usesVoice ? '' : 'none';
    if (custom) custom.style.display = (usesVoice && sel && sel.value === '__custom__') ? '' : 'none';
    if (note) {
      note.style.display = usesVoice ? 'none' : '';
      note.textContent = usesVoice ? '' : (engine === 'xiaozhi'
        ? '小智用服务端嗓音（在 xiaozhi.me 后台设置），这里无需选音色。'
        : '本地 Piper 由 sidecar 配置决定，这里无需选音色。');
    }
  }
  function currentVoiceValue() {
    const vsel = $('dev-voice-select');
    if (!vsel) return '';
    return vsel.value === '__custom__' ? (($('dev-voice').value) || '').trim() : vsel.value;
  }
  // 始终发完整三元组（后端按整组覆盖：空字段会清空，故不能只发其一）。
  function saveDevice() {
    const psrc = $('dev-persona-source');
    send(CliType.SetDevice, {
      persona: (($('dev-persona').value) || '').trim(),
      persona_source: psrc ? psrc.value : 'local',
      voice: currentVoiceValue(),
    });
  }
  function sceneNote(scene) {
    if (scene === 'web') return '单机网页：麦克风 + 播放都走浏览器（不会回音）。';
    if (scene === 'device') return '设备：麦克风/扬声器走 ElectronBot（树莓派经 sidecar / mpg123）。';
    return '当前为自定义组合（见下方「高级」）。同一台机器把输出设成「设备 + 网页」会回音。';
  }
  function updateSceneRadio(io) {
    let scene = '';
    if (io.audio_in === 'page' && io.audio_out === 'page') scene = 'web';
    else if (io.audio_in === 'device' && io.audio_out === 'device') scene = 'device';
    document.querySelectorAll('input[name="io-scene"]').forEach((r) => { r.checked = (r.value === scene); });
    const n = $('io-scene-note'); if (n) n.textContent = sceneNote(scene);
  }
  function setupIO() {
    IO_FIELDS.forEach((f) => {
      const sel = $('io-' + f);
      if (!sel) return;
      sel.addEventListener('change', () => {
        const payload = {}; payload[f] = sel.value;
        send(CliType.SetIO, payload);
        toast('已更新：' + f + ' = ' + sel.value);
        if (f === 'tts_engine') { updateVoiceVisibility(sel.value); setTimeout(loadVoices, 300); }
        if (f === 'audio_in' || f === 'audio_out') updateSceneRadio({ audio_in: $('io-audio_in').value, audio_out: $('io-audio_out').value });
      });
    });
    // 设备音量滑块：拖动实时更新数字，松手(change)发 set_volume 持久化并立即生效。
    const vol = $('io-device_volume'), volv = $('io-device_volume-val');
    if (vol) {
      vol.addEventListener('input', () => { if (volv) volv.textContent = vol.value; });
      vol.addEventListener('change', () => { send(CliType.SetVolume, { volume: parseInt(vol.value, 10) }); toast('设备音量 = ' + vol.value); });
    }
    // 舵机总开关：勾选即时下发（后端落盘 + 驱动下一帧生效）。开时提醒确认舵机 I²C 已通，
    // 否则主控固件会因对舵机无限重试而卡死整机（见 config.IO.ServoEnable 注释）。
    const servo = $('io-servo_enable');
    if (servo) {
      servo.addEventListener('change', () => {
        send(CliType.SetIO, { servo_enable: servo.checked });
        renderServoNote(servo.checked);
        toast(servo.checked ? '舵机已上扭矩（若舵机 I²C 未通，整机可能卡死）' : '舵机已卸力，可手动摆姿');
      });
    }
    // 安全退出：走 /api/shutdown（仅本机可调）→ 与 Ctrl+C 同一条优雅退出路径：驱动把手上这一帧
    // Sync 完整走完，再关设备。直接结束进程会把传输掐断在半帧中间，固件永久自旋、只能拔电源。
    const shut = $('btn-shutdown');
    if (shut) {
      shut.addEventListener('click', async () => {
        if (!confirm('关闭程序？会先把机器人安全断开（等当前帧发完），然后退出。')) return;
        shut.disabled = true;
        try {
          const r = await fetch('/api/shutdown', { method: 'POST' });
          toast(r.ok ? '正在安全退出…机器人已干净断开' : '退出失败：' + (await r.text()));
        } catch (e) {
          toast('正在安全退出…'); // 服务已关掉，连接被断开是预期的
        }
      });
    }
    // 使用场景预设：一键设 audio_in + audio_out。
    document.querySelectorAll('input[name="io-scene"]').forEach((r) => {
      r.addEventListener('change', () => {
        if (!r.checked) return;
        const s = SCENES[r.value]; if (!s) return;
        send(CliType.SetIO, s);
        const ai = $('io-audio_in'), ao = $('io-audio_out');
        if (ai) ai.value = s.audio_in; if (ao) ao.value = s.audio_out;
        const n = $('io-scene-note'); if (n) n.textContent = sceneNote(r.value);
        toast('已切换场景：' + (r.value === 'web' ? '单机网页' : '机器人设备'));
      });
    });
    // 声音：音色选择即时保存（发完整三元组），自定义 ID 失焦时保存。
    const vsel = $('dev-voice-select');
    if (vsel) vsel.addEventListener('change', () => {
      updateVoiceVisibility();
      if (vsel.value !== '__custom__') saveDevice();
    });
    const vcustom = $('dev-voice');
    if (vcustom) vcustom.addEventListener('change', saveDevice);
    const vref = $('dev-voice-refresh');
    if (vref) vref.addEventListener('click', (e) => { e.preventDefault(); loadVoices(); });
    // 角色/人设：来源切换即时保存并切换可见性；人设文本点「保存人设」生效。
    const psrc = $('dev-persona-source');
    if (psrc) psrc.addEventListener('change', () => {
      const prow = $('dev-persona-row');
      if (prow) prow.style.display = (psrc.value === 'model') ? 'none' : '';
      saveDevice();
    });
    setupVoicePreview();
    setupVoiceClone();
    const save = $('dev-save');
    if (save) save.addEventListener('click', () => { saveDevice(); toast('已保存人设'); });
    loadVoices(); // 初次加载当前引擎的音色列表
    updateVoiceVisibility();
  }

  // ---- 实时语音对话配置（设置页；开关即时生效，文本字段点保存下发）----
  // 后端把空字符串当作"不改动"，api_key 更是从不回传明文，故：文本框留空=保留原值；
  // 状态里只有 has_key 布尔，用它决定 key 输入框的占位提示。
  function renderRealtime(rt) {
    const en = $('rt-enabled');
    if (en) en.checked = !!rt.enabled;
    const prov = $('rt-provider');
    if (prov && rt.provider) prov.value = rt.provider;
    const setIfIdle = (id, v) => {
      const el = $(id);
      if (el && document.activeElement !== el && typeof v === 'string') el.value = v;
    };
    setIfIdle('rt-ws_base', rt.ws_base);
    setIfIdle('rt-model', rt.model);
    setIfIdle('rt-voice', rt.voice);
    const key = $('rt-api_key');
    if (key && document.activeElement !== key) {
      key.value = ''; // 从不回填明文
      key.placeholder = rt.has_key ? '已配置（留空则不修改）' : '未配置，请填入 API Key';
    }
    const note = $('rt-note');
    if (note) note.textContent = rt.enabled
      ? '实时对话已开启：唤醒后走云端实时语音链路（' + (rt.model || rt.provider || '') + '）。'
      : '实时对话已关闭：唤醒后走本地 ASR + LLM + TTS 链路。';
  }
  function setupRealtime() {
    const en = $('rt-enabled');
    if (en) en.addEventListener('change', () => {
      send(CliType.SetRealtime, { enabled: en.checked });
      toast(en.checked ? '实时对话已开启' : '实时对话已关闭');
    });
    const save = $('rt-save');
    if (save) save.addEventListener('click', () => {
      const payload = {
        provider: (($('rt-provider') && $('rt-provider').value) || '').trim(),
        ws_base: (($('rt-ws_base') && $('rt-ws_base').value) || '').trim(),
        model: (($('rt-model') && $('rt-model').value) || '').trim(),
        voice: (($('rt-voice') && $('rt-voice').value) || '').trim(),
      };
      const key = (($('rt-api_key') && $('rt-api_key').value) || '').trim();
      if (key) payload.api_key = key; // 仅在填了新值时下发，避免清空已存 key
      send(CliType.SetRealtime, payload);
      if ($('rt-api_key')) $('rt-api_key').value = '';
      toast('实时对话配置已保存');
    });
  }

  // 音色下拉：从 /api/voices 拉当前引擎可用音色，含"默认/自定义"项。
  let devVoice = '';
  function reconcileVoiceSelect() {
    const sel = $('dev-voice-select');
    if (!sel) return;
    const inList = Array.from(sel.options).some((o) => o.value === devVoice && o.value !== '__custom__' && o.value !== '');
    if (devVoice && !inList) {
      sel.value = '__custom__';
      $('dev-voice-custom-row').style.display = '';
      if (document.activeElement !== $('dev-voice')) $('dev-voice').value = devVoice;
    } else {
      sel.value = devVoice || '';
      $('dev-voice-custom-row').style.display = 'none';
    }
  }
  async function loadVoices() {
    const sel = $('dev-voice-select');
    if (!sel) return;
    try {
      const r = await fetch('/api/voices');
      const d = await r.json();
      sel.innerHTML = '<option value="">（默认）</option>';
      (d.voices || []).forEach((v) => {
        const o = document.createElement('option');
        o.value = v.id; o.textContent = v.name + '（' + v.id + '）';
        sel.appendChild(o);
      });
      const cust = document.createElement('option');
      cust.value = '__custom__'; cust.textContent = '（自定义 / 手填 ID）';
      sel.appendChild(cust);
      if (!(d.voices || []).length) sel.title = '当前引擎无可列音色（本地 sidecar 由模型决定，可用自定义）';
      reconcileVoiceSelect();
      updateVoiceVisibility();
    } catch (e) { /* 离线/未配置时忽略 */ }
  }

  // 试听：用当前选中的音色合成一句样例播放。
  let previewAudio = null;
  function setupVoicePreview() {
    const btn = $('dev-voice-preview');
    if (!btn) return;
    btn.addEventListener('click', (e) => {
      e.preventDefault();
      const sel = $('dev-voice-select');
      const voice = sel.value === '__custom__' ? ($('dev-voice').value || '').trim() : sel.value;
      if (previewAudio) { try { previewAudio.pause(); } catch (_) {} }
      btn.textContent = '⏳ 试听中';
      previewAudio = new Audio('/api/voice-preview?voice=' + encodeURIComponent(voice) + '&t=' + Date.now());
      previewAudio.onended = previewAudio.onerror = () => { btn.textContent = '▶ 试听'; };
      previewAudio.play().catch(() => { btn.textContent = '▶ 试听'; toast('试听失败（检查 TTS 引擎/凭据）'); });
    });
  }

  // 克隆：录音或上传音频 → POST /api/voice-clone → 成功后刷新音色列表并选中新音色。
  // 注意：MiniMax voice_clone 只接受 mp3/m4a/wav。浏览器 MediaRecorder 多产出 webm/opus，
  // 会被拒（2013 invalid file ext），故录音走 Web Audio 采 PCM、停止时自编码为 16bit WAV。
  let cloneCtx = null, cloneNode = null, cloneStream = null, clonePCM = [], cloneSR = 0, cloneWav = null;
  // 将累积的 Float32 PCM 编码为单声道 16bit PCM WAV blob。
  function encodeWav(chunks, sampleRate) {
    let total = 0; chunks.forEach((c) => { total += c.length; });
    const buf = new ArrayBuffer(44 + total * 2), view = new DataView(buf);
    const wstr = (off, s) => { for (let i = 0; i < s.length; i++) view.setUint8(off + i, s.charCodeAt(i)); };
    wstr(0, 'RIFF'); view.setUint32(4, 36 + total * 2, true); wstr(8, 'WAVE');
    wstr(12, 'fmt '); view.setUint32(16, 16, true); view.setUint16(20, 1, true); view.setUint16(22, 1, true);
    view.setUint32(24, sampleRate, true); view.setUint32(28, sampleRate * 2, true);
    view.setUint16(32, 2, true); view.setUint16(34, 16, true);
    wstr(36, 'data'); view.setUint32(40, total * 2, true);
    let off = 44;
    chunks.forEach((c) => { for (let i = 0; i < c.length; i++) { const s = Math.max(-1, Math.min(1, c[i])); view.setInt16(off, s < 0 ? s * 0x8000 : s * 0x7fff, true); off += 2; } });
    return new Blob([view], { type: 'audio/wav' });
  }
  function stopCloneRec() {
    if (cloneNode) { try { cloneNode.disconnect(); } catch (e) {} cloneNode = null; }
    if (cloneStream) { cloneStream.getTracks().forEach((t) => t.stop()); cloneStream = null; }
    if (cloneCtx) { try { cloneCtx.close(); } catch (e) {} cloneCtx = null; }
  }
  function setupVoiceClone() {
    const recBtn = $('clone-record'), subBtn = $('clone-submit'), st = $('clone-status');
    if (!subBtn) return;
    if (recBtn) recBtn.addEventListener('click', async (e) => {
      e.preventDefault();
      if (cloneNode) { // 正在录 → 停止并编码
        stopCloneRec();
        cloneWav = encodeWav(clonePCM, cloneSR);
        const secs = clonePCM.reduce((n, c) => n + c.length, 0) / (cloneSR || 1);
        recBtn.textContent = '● 录音';
        st.textContent = '已录制 ' + secs.toFixed(1) + ' 秒，点“克隆”提交';
        return;
      }
      try {
        cloneStream = await navigator.mediaDevices.getUserMedia({ audio: true });
        cloneCtx = new (window.AudioContext || window.webkitAudioContext)();
        cloneSR = cloneCtx.sampleRate; clonePCM = []; cloneWav = null;
        const src = cloneCtx.createMediaStreamSource(cloneStream);
        cloneNode = cloneCtx.createScriptProcessor(4096, 1, 1);
        cloneNode.onaudioprocess = (ev) => { clonePCM.push(new Float32Array(ev.inputBuffer.getChannelData(0))); };
        src.connect(cloneNode); cloneNode.connect(cloneCtx.destination);
        recBtn.textContent = '■ 停止'; st.textContent = '录音中…（说 10 秒以上）';
      } catch (err) { stopCloneRec(); toast('无法录音：' + err.message); }
    });
    subBtn.addEventListener('click', async (e) => {
      e.preventDefault();
      const name = ($('clone-name').value || '').trim();
      if (!name) { toast('请先填音色名（字母）'); return; }
      let blob = null, fname = 'voice.wav';
      const f = $('clone-file').files[0];
      if (f) {
        // MiniMax 只收 mp3/m4a/wav，上传文件先校验扩展名，避免白跑一趟。
        if (!/\.(mp3|m4a|wav)$/i.test(f.name)) { toast('仅支持 mp3 / m4a / wav 文件'); return; }
        blob = f; fname = f.name;
      } else if (cloneWav) { blob = cloneWav; fname = 'voice.wav'; }
      if (!blob) { toast('请先录音或选择音频文件'); return; }
      st.textContent = '上传并克隆中…（约几十秒）';
      try {
        const r = await fetch('/api/voice-clone?name=' + encodeURIComponent(name) + '&file=' + encodeURIComponent(fname), { method: 'POST', body: blob });
        if (!r.ok) { st.textContent = '克隆失败：' + (await r.text()); return; }
        const d = await r.json();
        st.textContent = '✅ 克隆成功：' + d.voice_id;
        toast('音色克隆成功，已刷新列表');
        await loadVoices();
        const sel = $('dev-voice-select');
        if (sel) { sel.value = d.voice_id; $('dev-voice-custom-row').style.display = 'none'; }
      } catch (err) { st.textContent = '克隆请求失败'; }
    });
  }

  function renderSettings(s) {
    if (s.asr) el.setASR.textContent = (s.asr.running ? '在线' : '离线') + (s.asr.detail ? ` · ${s.asr.detail}` : '');
    if (s.tts) el.setTTS.textContent = (s.tts.running ? '在线' : '离线') + (s.tts.detail ? ` · ${s.tts.detail}` : '');
    if (s.robot) {
      el.setUSB.textContent = s.robot.recovering ? '卡死·自动软复位中(免拔电源)…'
        : s.robot.stuck ? '卡死(自动复位无效，需断电复位)'
        : (s.robot.connected ? ('已连接' + (s.robot.speed ? '（' + s.robot.speed + '）' : '')) : '未连接');
      el.setVidPid.textContent = `0x${(s.robot.vid || 0).toString(16)} / 0x${(s.robot.pid || 0).toString(16)}`;
      el.setFPS.textContent = (s.robot.fps || 0) + ' fps';
    }
  }

  // ---- 顶栏模型下拉 ----
  let activeModelId = ''; // 服务端当前生效模型；用于发送失败时把下拉纠回真实值
  function renderModelPicker(llm) {
    activeModelId = llm.active || '';
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
  // 一键蹦迪：toggle —— 开始放歌+跳舞；再点一下停舞(打断)+停歌。
  const partyBtn = $('btn-party');
  if (partyBtn) {
    let partyOn = false;
    partyBtn.addEventListener('click', () => {
      if (!partyOn) {
        send(CliType.Party, {});
        partyBtn.textContent = '⏹ 停止蹦迪';
        partyBtn.classList.add('danger');
        partyOn = true;
      } else {
        send(CliType.Interrupt, { reason: 'party-stop' });
        send(CliType.Music, { action: 'stop' });
        partyBtn.textContent = '🪩 蹦迪';
        partyBtn.classList.remove('danger');
        partyOn = false;
      }
    });
  }
  el.model.addEventListener('change', () => {
    // 发不出去（断线）就把下拉纠回服务端真实值，避免顶栏显示假状态。
    if (!send(CliType.SelectModel, { id: el.model.value })) el.model.value = activeModelId;
  });

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
    } else if (audioInMode === 'network') {
      toggleNetworkRecord(); // 网页录音 → 网络 ASR（点击开始/再点结束）
    } else {
      micOn = !micOn;
      el.mic.classList.toggle('active', micOn);
      send(CliType.Mic, { action: micOn ? 'start' : 'stop' });
    }
  });

  // ---- 网络 ASR：网页录音(MediaRecorder) → POST /api/transcribe → 当作一次对话输入 ----
  let mediaRec = null, recChunks = [];
  async function toggleNetworkRecord() {
    if (mediaRec && mediaRec.state === 'recording') { mediaRec.stop(); return; }
    try {
      const stream = await navigator.mediaDevices.getUserMedia({ audio: true });
      recChunks = [];
      mediaRec = new MediaRecorder(stream);
      mediaRec.ondataavailable = (e) => { if (e.data && e.data.size) recChunks.push(e.data); };
      mediaRec.onstop = async () => {
        stream.getTracks().forEach((t) => t.stop());
        el.mic.classList.remove('active');
        setVoice('thinking');
        const blob = new Blob(recChunks, { type: mediaRec.mimeType || 'audio/webm' });
        try {
          const ext = (mediaRec.mimeType || 'audio/webm').includes('ogg') ? 'ogg' : 'webm';
          const r = await fetch('/api/transcribe?name=audio.' + ext, { method: 'POST', body: blob });
          if (!r.ok) { toast('识别失败：' + (await r.text())); setVoice('idle'); return; }
          const d = await r.json();
          if (d.text) { send(CliType.SendText, { text: d.text }); }
          else { toast('没听清，请再说一遍'); setVoice('idle'); }
        } catch (e) { toast('识别请求失败'); setVoice('idle'); }
      };
      mediaRec.start();
      el.mic.classList.add('active');
      setVoice('listening');
      toast('录音中，再点一下麦克风结束');
    } catch (e) { toast('无法访问麦克风：' + e.message); }
  }

  // ---- 音频播放（页面调试镜像：后端把合成语音 base64 推来，浏览器播放）----
  let currentAudio = null;
  let musicDucked = false;     // 因播放语音回复而临时暂停了音乐，待回复结束续播
  function stopAudio() { if (currentAudio) { try { currentAudio.pause(); } catch (_) {} currentAudio = null; } }
  // 语音回复结束/被打断后：续播音乐 + 让语音状态回到待命（与真实播放同步）。
  function endVoicePlayback() {
    if (musicDucked && musicAudio) { musicAudio.play().catch(() => {}); }
    musicDucked = false;
    if (pageVoiceActive) { pageVoiceActive = false; setVoice('idle'); }
  }
  function duckMusicForVoice() {
    if (musicAudio && !musicAudio.paused) { try { musicAudio.pause(); } catch (_) {} musicDucked = true; }
  }

  // ---- 流式分段语音（小智逐句）：用 Web Audio 按时间轴无缝排期，避免 <audio> 段间停顿 ----
  let actx = null;             // AudioContext
  let streamNextStart = 0;     // 下一段应开始的精确时间（contiguous 排期）
  let streamSources = [];      // 已排期的源（用于打断时停止）
  let streamChain = Promise.resolve(); // 串行解码链：严格按到达顺序解码+排期，避免乱序丢段
  let streamEndTimer = null;
  function ensureCtx() {
    if (!actx) actx = new (window.AudioContext || window.webkitAudioContext)();
    if (actx.state === 'suspended') actx.resume().catch(() => {});
    return actx;
  }
  function stopStreamAudio() {
    streamSources.forEach(s => { try { s.stop(); } catch (_) {} });
    streamSources = [];
    streamNextStart = 0;
    streamChain = Promise.resolve();
    if (streamEndTimer) { clearTimeout(streamEndTimer); streamEndTimer = null; }
  }
  function scheduleStreamChunk(b64) {
    const ctx = ensureCtx();
    const bytes = Uint8Array.from(atob(b64), c => c.charCodeAt(0));
    // decodeAudioData 用 Promise 形式；老实现回调形式这里统一包一层。
    return new Promise(resolve => {
      const done = () => resolve();
      let p;
      try { p = ctx.decodeAudioData(bytes.buffer); } catch (_) { return done(); }
      if (!p || !p.then) { return done(); } // 不支持 Promise 形式则跳过该段
      p.then(buf => {
        const src = ctx.createBufferSource();
        src.buffer = buf;
        src.connect(ctx.destination);
        const startAt = Math.max(ctx.currentTime + 0.03, streamNextStart); // 30ms 缓冲避免欠载
        try { src.start(startAt); } catch (_) { return done(); }
        streamNextStart = startAt + buf.duration;
        streamSources.push(src);
        src.onended = () => {
          streamSources = streamSources.filter(s => s !== src);
          // 全部播完且没有后续排期 → 回到待命（留一点余量给在途解码）。
          if (streamEndTimer) clearTimeout(streamEndTimer);
          streamEndTimer = setTimeout(() => {
            if (streamSources.length === 0 && streamNextStart <= ctx.currentTime + 0.05) {
              streamNextStart = 0;
              endVoicePlayback();
            }
          }, 120);
        };
        done();
      }).catch(() => done());
    });
  }
  function onAudio(p) {
    if (!p) return;
    if (p.stop) { stopStreamAudio(); stopAudio(); endVoicePlayback(); return; } // 打断：停流式+单段、回待命
    if (p.stream && p.data) {
      // 流式分段（小智逐句）：无缝排期，严格按到达顺序解码。
      duckMusicForVoice();
      pageVoiceActive = true;
      setVoice('speaking');
      streamChain = streamChain.then(() => scheduleStreamChunk(p.data)).catch(() => {});
      return;
    }
    // 非流式（音乐 URL / 整段语音）：停掉当前再播单段。
    stopStreamAudio();
    stopAudio();
    let src = p.url;
    if (!src && p.data) {
      const mime = (!p.format || p.format === 'mp3') ? 'mpeg' : p.format;
      src = 'data:audio/' + mime + ';base64,' + p.data;
    }
    if (!src) return;
    duckMusicForVoice();
    pageVoiceActive = true;
    setVoice('speaking');
    try {
      currentAudio = new Audio(src);
      currentAudio.addEventListener('ended', endVoicePlayback);
      currentAudio.addEventListener('error', endVoicePlayback);
      currentAudio.play().catch(() => endVoicePlayback());
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
  setupRealtime();
  connect();
})();
