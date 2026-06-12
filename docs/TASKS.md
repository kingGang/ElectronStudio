# 天气 / 提醒 / 定时任务

## 天气

`get_weather` 工具（基于免 key 的 wttr.in，纯 Go HTTP）：说"今天北京天气怎么样"，
大模型会调用工具返回当前天气。需生效模型支持工具调用。

## 提醒 / 闹钟 / 定时任务

统一由调度器（`internal/scheduler`）管理，任务存盘于 `jobs.json`（与 config 同目录），重启恢复。
一个任务三选一触发方式：

| 方式 | 字段 | 例 |
|------|------|----|
| 一次性（提醒/闹钟） | `at` (RFC3339) | `2026-06-12T08:00:00+08:00` |
| 每日定时 | `daily` (HH:MM) | `08:00` |
| 周期 | `every` (时长) | `1h` / `30m` |

到点执行其动作（`kind`）：
- `say`：机器人说出 `text`
- `weather`：播报 `query` 城市天气
- `greet`：看一眼打招呼
- `music`：播放 `query`

### 用法

- **前端**：设置页"提醒/定时任务"面板——填标题 + 选方式（N 分钟后 / 每日 HH:MM / 每隔）+ 值 → 添加；列表可删除。
- **大模型工具 `set_reminder`**：说"10 分钟后提醒我喝水"，模型创建一次性提醒，到点机器人会说出来。
- **命令**：`schedule_add {title, at|every|daily, kind, text, query}` / `schedule_remove {id}`；列表通过 `schedule_list` 事件下发。
