# 工具脚本

## pack_gifs —— GIF 批量打包成表情图集

把一堆 GIF（一个情绪一个文件）批量打包成 ElectronStudio 用的精灵图集
（TexturePacker `JSON (Array)`），自动抽帧、取帧率、生成 `<情绪>.png` + `<情绪>.json`。

### 依赖

- **ffmpeg + ffprobe**：抽帧、取 GIF 帧率
- **TexturePacker（命令行）**：打包图集（需在 PATH；`TexturePacker --help` 可验证）

### 用法

```bash
# Linux / macOS
scripts/pack_gifs.sh ./gifs ./emotions

# Windows
pwsh scripts/pack_gifs.ps1 -Src .\gifs -Out .\emotions
```

输入目录里放 `happy.gif`、`sad.gif`、`thinking.gif`…（**文件名 = 情绪名**），
输出到 `emotions/`：每个生成 `<情绪>.png` + `<情绪>.json`。

### 行为

- 每帧缩放到 **240×240**（圆屏尺寸）。
- 帧率 fps 自动取自 GIF（`avg_frame_rate`），写入 json 顶层 `"fps"`；取不到则默认 15。
- TexturePacker 用 `--format json-array --trim-mode Trim --disable-rotation`
  （加载器其实也支持旋转，默认禁用最省心；要更紧凑可去掉该参数）。

### 接入

把生成的 `emotions/` 放到与 `config.json` **同目录**即可。某情绪有图集就播图集，
没有就回退程序动画脸。详见 [`../docs/EMOTIONS.md`](../docs/EMOTIONS.md)。
