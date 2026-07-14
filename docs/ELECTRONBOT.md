# ElectronBot 真机接入

`internal/robot/electronbot` 通过 USB 直连稚晖君的 [ElectronBot](https://github.com/peng-zhihui/ElectronBot)，
实现 `robot.Transport`。为保持主程序纯 Go（无 cgo、可交叉编译），它用
[purego](https://github.com/ebitengine/purego) 在运行时动态加载 **libusb**，而非链接
官方的 Windows 专用 `USBInterface.dll`。

## 协议（对照官方 electron_low_level.cpp，字节级一致）

- 设备：VID `0x1001` / PID `0x8023`；bulk 端点 `EP1_OUT(0x01)` 写、`EP1_IN(0x81)` 读，接口 0。
- 一次 `Sync` = 循环 **4 段**，每段：
  1. 从 `EP1_IN` 读 **32 字节**（MCU 请求 + 舵机角度反馈）；
  2. 向 `EP1_OUT` 写 **84 × 512 = 43008 字节** 图像主体；
  3. 向 `EP1_OUT` 写 **224 字节**（= 192 字节图像尾 + 32 字节 extraData）。
- 图像：240×240×3 = **172800 字节 RGB888**。
- extraData（32B）：`[0]` 使能位，`[1+4j..]` 第 j 轴 `float32`（小端），共 6 轴。
- 反馈（32B）：同布局，`Sync` 后由 `JointAngles()` 解析。

## 启用

配置项 `robot`（`config.json`）：

| 值 | 行为 |
|----|------|
| `auto`（默认） | 尝试连真机，未检测到则静默回退 Mock |
| `electronbot` | 强制连真机，失败告警后回退 Mock |
| `mock` | 始终用 Mock |

默认 `auto` 即“插上即用”：装好 libusb、接上 ElectronBot，启动即自动连接。

## 各平台前置：安装 libusb（运行期依赖，非编译期）

| 平台 | 步骤 |
|------|------|
| **Linux / 树莓派** | `sudo apt install libusb-1.0-0`；建议加 udev 规则免 root：`SUBSYSTEM=="usb", ATTrs{idVendor}=="1001", ATTRS{idProduct}=="8023", MODE="0666"` |
| **macOS** | `brew install libusb` |
| **Windows** | `scripts/run.ps1` 会自动下载 `libusb-1.0.dll` 到仓库根目录（缺则下，已有则跳过）。手动装的话：从 [libusb releases](https://github.com/libusb/libusb/releases) 取 `VS2022/MS64/dll/libusb-1.0.dll` 放到**可执行文件同目录或工作目录**。驱动方面，免驱固件（精英版 5241:5241）自带 WinUSB 即插即用；初代（1001:8023）需用 [Zadig](https://zadig.akeo.ie/) 装 **WinUSB** 驱动 |

> 主程序自身用 `CGO_ENABLED=0` 交叉编译，不需要 C 工具链；libusb 仅在运行时按设备需要加载。
>
> **`libusb-1.0.dll` 不入库**（`.gitignore` 里有 `*.dll`）。它一旦缺失，`electronbot.Probe()` 会**静默失败并回退 Mock**——现象是"机器人明明插着、驱动也正常，程序却说没探测到"，日志里一个字都没有。排查真机连不上时，**先确认这个 dll 在不在**。

## USB 拓扑：一线多设备

ElectronBot 内部有一颗 **4 口 USB Hub**，一根 USB 线插进去，主机看到的是多个**标准 USB 设备**：

| 设备 | 类型 | 主机如何访问 |
|------|------|------------|
| STM32 屏幕 + 舵机 | 自定义（VID 0x1001 / PID 0x8023） | 本包实现的 USB 协议（屏幕只写 + 32B 舵机反馈，**无音频**） |
| 摄像头 | 标准 UVC | 当普通 webcam 直接读（可作画面源推到屏幕，见 docs/EMOTIONS.md 的可插拔 Source） |
| 麦克风（预留口，需自加） | 标准 UAC | 当普通麦克风读；语音 sidecar 选中该设备即可（见 sidecars/speech） |

要点：**摄像头/麦克风不走 0x8023 自定义协议**，它们是 hub 上的独立标准 USB 设备，主机用通用方式访问。
base ElectronBot 本身无麦克风/喇叭，板上预留了一个 USB 口供加 USB 麦。

## 6 自由度关节（顺序 = 官方下发给固件的线上顺序）

ElectronBot 是 **6 自由度**：每臂 2 个（横滚 + 俯仰），头 1 个（俯仰），身体 1 个（偏航）——**没有肘关节**。下标顺序即 `Joints`：

| 下标 | 关节 | 轴 |
|------|------|----|
| 0 | 头部俯仰 | X |
| 1 | 左臂横滚 | Z |
| 2 | 左臂俯仰 | X |
| 3 | 右臂横滚 | Z（取反） |
| 4 | 右臂俯仰 | X |
| 5 | 身体旋转 | Y（偏航） |

顺序依据官方上位机 `3.Software/Unity/ElectronBot-Studio/Assets/Scripts/UnityGetImageFromCpp.cs`（`joints[0]=sliderAngleHead` … `joints[5]=sliderAngleBody`）。**不要照 `RobotController.cs` 的字段声明顺序**——那只是 UI 字段的书写次序、不是线上顺序，照抄会让头与手臂整体错位一格（舵机动的关节和界面对不上）。

内置动作（wave/nod/shake/cheer/home）按此布局编排；角度幅度/方向可在真机上微调。

## 动作编排 / 示教录制

对应官方"实体优先"同步模式：
- **跟随设备**：后台以 20Hz 让舵机松力（`enable=false`）并读回真实角度，掰动机器人时界面滑块实时跟随。
- **示教录制**：录制中点"采帧"把当前姿态存为关键帧，停止即保存为新动作到 `actions.json`（与 `config.json` 同目录），下次启动自动加载。
- 动作播放沿用关键帧线性插值；可在设置/动作页删除已录动作。

## 屏幕画面与同步

设备屏是"只写"的（USB 上行只有 32 字节舵机反馈，无法回读画面），所以**主机是唯一帧源**：
- `internal/display` 提供画面源 `Source`（当前为表情渲染 `EmotionSource`，可插拔为摄像头源）。
- `internal/device.Driver` 以固定帧率（30fps）拥有设备 `Sync`：把"当前姿态 + 当前画面"一并发给设备，
  并把**同一帧**通过 `BroadcastFrame` 广播给 UI 圆屏镜像——因此**设备屏与界面镜像逐字节同步**。
- 动作编排不再各自 `Sync`，只把目标姿态写入驱动，避免图像/姿态错位。

摄像头画面同理：帧先到主机（host webcam / ffmpeg sidecar / 树莓派 V4L2），再由驱动同时推设备屏 + UI。

## 真机踩坑实录（2026-07 精英版实测，看完能省一天）

### 1. 舵机跑着跑着一颗颗"死掉"——是【舵机固件】的死锁，主机治不了

**现象**：上电后十几分钟内，某一轴的角度反馈开始**恒为一个定值、一个比特都不变**，同时那颗舵机
**不再响应任何命令**。一颗接一颗地死，顺序随机。**只有断电（拔线 ≥15 秒）能全部复活**——重启程序、
USB 复位、重新使能，统统无效。

**根因在舵机自己的固件里**（`peng-zhihui/ElectronBot` → `2.Firmware/ServoDrive-fw/UserApp/main.cpp`）。
每颗舵机是一块自制智能舵机板：STM32F0 + FM116B H 桥 + 电位器 ADC，作为 I²C 从机挂在主控总线上。
它的 I²C 命令处理**跑在中断里**，却调用了**阻塞** HAL 函数，还套了**无限重试**：

	void HAL_I2C_SlaveRxCpltCallback(I2C_HandleTypeDef* hi2c)   // ← 中断上下文
	{
	    do {
	        state = HAL_I2C_Slave_Transmit(&hi2c1, i2cDataTx, 5, 10000);  // ← 阻塞，超时 10s
	    } while (state != HAL_OK);                                        // ← 无限重试
	}

致命的一环是中断优先级：**I2C1 = 0，SysTick 也 = 0**（`TICK_INT_PRIORITY`），而 Cortex-M0 上
**同优先级异常不能互相抢占**——进了 I²C 中断，SysTick 就永远打不进来 → `HAL_GetTick()` 冻结 →
而那个 10 秒超时判的正是 `HAL_GetTick() - tickstart > Timeout` → **超时永远不会到期**。

于是**任何一次 I²C 抖动**（NACK / 毛刺 / 一个比特翻转）→ 舵机 MCU **永久卡死在中断里** →
200Hz 控制环（TIM14，优先级 3）再也轮不到 → `motor.angle` 停更、`SetPwm()` 不再调用。
但 I²C 从机**硬件**仍在应答，把陈旧的 float 原样回传。**舵机 MCU 没有看门狗，所以只能断电。**

**怎么一眼确认是它**：精英版主控固件（`maker-community/ElectronBot-fw` @ `dev_winusb`，
`Bsp/robot.cpp`）在 I²C 失败时会把角度写成**精确的 0**：

	if (TransmitAndReceiveI2cPacket(_joint.id) == true) { _joint.angle = *(float*)(i2cRxData+1); }
	else { _joint.angle = 0; }

所以——**冻结值非零 = I²C 事务仍然成功 = 总线是好的，死的是舵机内部的控制环**（本 bug）；
**冻结值恰好是 0 = 主控读不到它 = 那才是总线/接线问题**。

**排除过的错误方向**（都被实测推翻，别再走）：不是堵转、不是发热、不是供电塌陷、不是总线被拉死、
不是舵机物理损坏。决定性实验：**把舵机总开关关掉（零扭矩、零电流）跑 30 分钟，照样 9 分钟内冻 4 颗**
——触发条件是**总线事务**，与力矩完全无关。

**真正的修法**（要拆机刷 6 颗舵机的 SWD）：
1. 把 I²C 命令处理从中断搬到主循环（[Issue #131](https://github.com/peng-zhihui/ElectronBot/issues/131)
   里 jinsonli 2022 年就报了这个 bug 并给了修好的固件，**至今 OPEN、上游从未合并**）；
2. 或最小补丁：`I2C1_IRQn` 优先级 `0 → 1`（让 SysTick 能抢占，10s 超时才会真的生效）+ 删掉
   `do/while` 无限重试 + **加 IWDG 看门狗**（死锁自动复位，连断电都省了）。
3. 硬件缓解：舵机 I²C 只靠 STM32 内部 ~40kΩ 弱上拉，6 从机一条总线上升沿很慢、极易产生毛刺——
   那就是扳机。加**外部 2.2k~4.7kΩ 上拉到 3.3V** 可显著降低触发率。

**主机侧唯一能做的止血**（尚未实现）：精英版主控有这个分支——

	if (_angleSetPoint >= _joint.angleMin && _angleSetPoint <= _joint.angleMax) { ...I²C... }

**目标角越界（如 NaN）→ 主控跳过这颗舵机的 I²C**。所以不用舵机时给 6 轴发 NaN，可让舵机
**零 I²C 流量、物理上不可能死锁**。注意唤醒时必须先用合法角度 + `enable=0` 读回真实位置、把目标角
对齐到当前位置，再上扭矩——否则 0→1 那一跳会给出一个"离当前位置很远"的目标 → 电流尖峰 → 舵机
不 ACK → 正好踩中上面那个死锁（见 `protocol.go` 的 `buildExtraData` 注释，这个坑踩过一次）。

### 2. `joint_trim` 必须逐台标定，全 0 会把舵机顶死

`joint_trim`（机械零位补偿）默认全 0，等于宣称"每颗舵机的机械零位正好是 0°"——**装配公差决定了这
基本不可能**。本机实测：两颗**横滚**舵机的机械限位在 **+5.25° / +7.81°**，物理上**下不到 0°**。

而驱动的静止姿态是"6 轴全 0"。于是**舵机一使能，这两颗就被命令去一个够不到的位置、然后 24 小时
死顶机械限位**——机器人看着一动不动，它俩却在满负荷堵转。

**标定流程**（每台机器都要做一次）：
1. 关掉舵机总开关（不上扭矩，可手动摆姿）；
2. 用手把机器人摆成端正姿势；
3. 读回 6 轴角度——但**只给"真的够不到 0"的轴写 trim**。判据是：舵机**使能**、命令它去 0° 时，
   它实际停在哪。能到 0 的轴 trim 留 0；停在 X° 下不去的轴，trim ≈ X + 2~3°（留余量，别贴着限位）。
   ⚠️ **别直接拿"无扭矩时的静止读数"当 trim**——那里面混着重力下垂（本机头部无扭矩时垂到 16.8°，
   但它上扭矩后明明能到 0）。
4. 标定后静止时 6 轴的跟随误差应全部 < 3°，说明没有任何一颗在死顶。

### 3. 强杀进程 = 掐死固件 = 必须拔电源

固件的一帧是 4 段严格 lockstep，收发全是**无超时自旋**。进程被 `Stop-Process -Force` / 任务管理器
结束 / 崩溃时，`Close()` 的优雅退出**根本没机会跑**（TerminateProcess 不可捕获），传输断在半帧中间
→ MCU 永远停在 `while(receivedPacketLen != 224)` 上等那个再也不会来的尾包 → 只能断电。

**判据**：连上后**第一次 IN 读就超时** = 设备带着旧伤来的，不是本次运行搞坏的。

**所以停程序只能走这三条路**：`Ctrl+C`、设置页的「安全退出」按钮、`POST /api/shutdown`（仅回环）。
三者走的是同一条路径：等驱动把手上这一帧 Sync 完整走完，再关设备。

## 说明

- 无法在无真机环境端到端验证舵机/屏幕；分段/打包/角度编解码、画面同步管线已有单元测试与本机联调覆盖，协议与官方 SDK 逐字对齐。
