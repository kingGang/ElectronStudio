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
| **Windows** | 安装 libusb 运行库（提供 `libusb-1.0.dll`，可随程序分发到同目录）；并用 [Zadig](https://zadig.akeo.ie/) 给 ElectronBot 安装 **WinUSB** 驱动 |

> 主程序自身用 `CGO_ENABLED=0` 交叉编译，不需要 C 工具链；libusb 仅在运行时按设备需要加载。

## USB 拓扑：一线多设备

ElectronBot 内部有一颗 **4 口 USB Hub**，一根 USB 线插进去，主机看到的是多个**标准 USB 设备**：

| 设备 | 类型 | 主机如何访问 |
|------|------|------------|
| STM32 屏幕 + 舵机 | 自定义（VID 0x1001 / PID 0x8023） | 本包实现的 USB 协议（屏幕只写 + 32B 舵机反馈，**无音频**） |
| 摄像头 | 标准 UVC | 当普通 webcam 直接读（可作画面源推到屏幕，见 docs/EMOTIONS.md 的可插拔 Source） |
| 麦克风（预留口，需自加） | 标准 UAC | 当普通麦克风读；语音 sidecar 选中该设备即可（见 sidecars/speech） |

要点：**摄像头/麦克风不走 0x8023 自定义协议**，它们是 hub 上的独立标准 USB 设备，主机用通用方式访问。
base ElectronBot 本身无麦克风/喇叭，板上预留了一个 USB 口供加 USB 麦。

## 6 自由度关节（与官方 ElectronStudio 的 RobotController 一致）

ElectronBot 是 **6 自由度**：每臂 2 个（横滚 + 俯仰），头 1 个（俯仰），身体 1 个（偏航）——**没有肘关节**。下标顺序即 `Joints`：

| 下标 | 关节 | 轴 |
|------|------|----|
| 0 | 左臂横滚 | Z |
| 1 | 左臂俯仰 | X |
| 2 | 右臂横滚 | Z（取反） |
| 3 | 右臂俯仰 | X |
| 4 | 头部俯仰 | X |
| 5 | 身体旋转 | Y（偏航） |

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

## 说明

- 无法在无真机环境端到端验证舵机/屏幕；分段/打包/角度编解码、画面同步管线已有单元测试与本机联调覆盖，协议与官方 SDK 逐字对齐。
