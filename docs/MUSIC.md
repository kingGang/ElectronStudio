# 音乐

受"主程序无 cgo"约束：**搜索走纯 Go HTTP（酷我），播放交给外部 `mpg123` 子进程**
（`mpg123 -R` 远程模式，可控暂停/停止/音量，与原 Verdure 项目一致）。

## 前置

```bash
# Linux/树莓派
sudo apt install mpg123
# macOS
brew install mpg123
# Windows: 下载 mpg123 放入 PATH，或在 config.json 指定路径
```

`config.json`（可选）：

```jsonc
"music": { "mpg123": "mpg123" }   // 自定义 mpg123 路径
```

## 用法

- **前端**：首页底部音乐条——输入歌名/歌手 → ▶ 播放，⏸ 暂停/继续，⏹ 停止；正在播放会显示曲名。
- **大模型工具 `play_music`**：说"放首稻香"，模型会调用工具搜索并播放。
- **命令**：`music {action: play|pause|resume|stop|volume, query, volume}`。

## 说明

- 酷我为非官方 Web 接口（依赖 csrf token），可能随其改版失效——属尽力实现，失效时按最新接口微调
  `internal/music/kuwo.go`（解析逻辑有单元测试）。可实现 `music.Searcher` 接口接入其它音源。
- 播放控制（暂停/停止/音量）经 mpg123 远程命令；状态变化通过 `music_state` 事件广播给 UI。
