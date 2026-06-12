// Package scheduler 提供提醒/闹钟（一次性）与定时任务（周期）的统一调度。
//
// 一个 Job 可按以下之一触发：
//   - At    "2006-01-02T15:04:05Z07:00"：一次性绝对时间（提醒/闹钟）
//   - Every "1h" / "30m"：周期间隔
//   - Daily "08:00"：每日定时
//
// 到点时调用 fire 回调，由上层执行其 Action（说一句 / 报天气 / 打招呼 / 放歌）。
// Job 列表可存盘（jobs.json），重启后恢复。
package scheduler

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"sort"
	"strconv"
	"sync"
	"time"
)

// Action 描述任务触发时要做什么。
type Action struct {
	Kind  string `json:"kind"`            // say | weather | greet | music
	Text  string `json:"text,omitempty"`  // say：要说的话
	Query string `json:"query,omitempty"` // weather：城市；music：歌名
}

// Job 是一个调度任务。
type Job struct {
	ID     string `json:"id"`
	Title  string `json:"title"`
	At     string `json:"at,omitempty"`    // RFC3339 一次性
	Every  string `json:"every,omitempty"` // 时长字符串，周期
	Daily  string `json:"daily,omitempty"` // HH:MM 每日
	Action Action `json:"action"`

	next time.Time // 内部：下次触发时间（不序列化）
}

// Scheduler 管理并触发 Job。
type Scheduler struct {
	mu   sync.Mutex
	jobs map[string]*Job
	seq  int
	fire func(Job)
	log  *slog.Logger
	tick time.Duration
	path string // 非空则在增删/触发后自动存盘
}

// SetPath 设置自动存盘路径（在 Load 之后调用，避免加载过程反复写盘）。
func (s *Scheduler) SetPath(p string) { s.path = p }

// persist 在未持锁时调用：把当前任务写盘。
func (s *Scheduler) persist() {
	if s.path == "" {
		return
	}
	if err := s.Save(s.path); err != nil {
		s.log.Warn("保存定时任务失败", "err", err)
	}
}

// New 创建调度器。fire 在任务到点时被调用。
func New(fire func(Job), log *slog.Logger) *Scheduler {
	if log == nil {
		log = slog.Default()
	}
	return &Scheduler{jobs: make(map[string]*Job), fire: fire, log: log, tick: time.Second}
}

// Add 添加一个任务，计算其首次触发时间，返回其 ID（为空则自动生成）。
func (s *Scheduler) Add(j Job) (string, error) {
	next, err := nextFire(j, time.Now())
	if err != nil {
		return "", err
	}
	s.mu.Lock()
	if j.ID == "" {
		s.seq++
		j.ID = "job" + strconv.Itoa(s.seq)
	}
	j.next = next
	jc := j
	s.jobs[j.ID] = &jc
	s.mu.Unlock()
	s.persist()
	return j.ID, nil
}

// Remove 删除一个任务。
func (s *Scheduler) Remove(id string) {
	s.mu.Lock()
	delete(s.jobs, id)
	s.mu.Unlock()
	s.persist()
}

// List 返回全部任务（按标题排序的副本）。
func (s *Scheduler) List() []Job {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]Job, 0, len(s.jobs))
	for _, j := range s.jobs {
		out = append(out, *j)
	}
	sort.Slice(out, func(i, k int) bool { return out[i].Title < out[k].Title })
	return out
}

// Run 启动调度循环，阻塞直到 ctx 取消。
func (s *Scheduler) Run(ctx context.Context) {
	ticker := time.NewTicker(s.tick)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case now := <-ticker.C:
			s.checkDue(now)
		}
	}
}

// checkDue 触发所有到点任务，并重排（周期）或移除（一次性）。
func (s *Scheduler) checkDue(now time.Time) {
	s.mu.Lock()
	var due []Job
	for id, j := range s.jobs {
		if !j.next.IsZero() && !now.Before(j.next) {
			due = append(due, *j)
			// 重排或移除。
			if j.Every != "" || j.Daily != "" {
				if n, err := nextFire(*j, now.Add(time.Second)); err == nil {
					j.next = n
				} else {
					delete(s.jobs, id)
				}
			} else {
				delete(s.jobs, id) // 一次性
			}
		}
	}
	s.mu.Unlock()

	if len(due) > 0 {
		s.persist() // 触发后任务集变化（一次性移除/周期重排），落盘
	}
	for _, j := range due {
		s.log.Info("触发任务", "title", j.Title, "kind", j.Action.Kind)
		if s.fire != nil {
			s.fire(j)
		}
	}
}

// nextFire 计算 j 相对 from 的下次触发时间。
func nextFire(j Job, from time.Time) (time.Time, error) {
	switch {
	case j.At != "":
		t, err := time.Parse(time.RFC3339, j.At)
		if err != nil {
			return time.Time{}, fmt.Errorf("scheduler: 非法时间 %q: %w", j.At, err)
		}
		return t, nil
	case j.Every != "":
		d, err := time.ParseDuration(j.Every)
		if err != nil || d <= 0 {
			return time.Time{}, fmt.Errorf("scheduler: 非法间隔 %q", j.Every)
		}
		return from.Add(d), nil
	case j.Daily != "":
		hh, mm, err := parseHM(j.Daily)
		if err != nil {
			return time.Time{}, err
		}
		t := time.Date(from.Year(), from.Month(), from.Day(), hh, mm, 0, 0, from.Location())
		if !t.After(from) {
			t = t.Add(24 * time.Hour)
		}
		return t, nil
	default:
		return time.Time{}, errors.New("scheduler: 任务缺少触发时间(at/every/daily)")
	}
}

func parseHM(s string) (int, int, error) {
	var hh, mm int
	if _, err := fmt.Sscanf(s, "%d:%d", &hh, &mm); err != nil || hh < 0 || hh > 23 || mm < 0 || mm > 59 {
		return 0, 0, fmt.Errorf("scheduler: 非法每日时间 %q (应为 HH:MM)", s)
	}
	return hh, mm, nil
}

// Save 原子写入任务到 path。
func (s *Scheduler) Save(path string) error {
	data, err := json.MarshalIndent(s.List(), "", "  ")
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// Load 从 path 读取任务并加入；文件不存在时静默返回。
func (s *Scheduler) Load(path string) error {
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	var jobs []Job
	if err := json.Unmarshal(data, &jobs); err != nil {
		return err
	}
	for _, j := range jobs {
		if _, err := s.Add(j); err != nil {
			s.log.Warn("跳过非法任务", "title", j.Title, "err", err)
		}
	}
	return nil
}
