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

## 说明

- 当前动作编排只驱动舵机，屏幕画面默认为黑（`SetImage` 已就绪，后续可把表情帧推上去）。
- 无法在无真机环境端到端验证；分段/打包/角度编解码已有单元测试覆盖，协议与官方 SDK 逐字对齐。
