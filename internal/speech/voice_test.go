package speech

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"
)

// fakeSidecar 是一个最小的假 sidecar：像真货一样，收到 set_voice 就回一条 voice 上报。
// 这个"命令 → 回执"的行为正是反馈环的另一半，测试必须复刻它，否则环根本不会出现。
type fakeSidecar struct {
	srv      *httptest.Server
	setVoice atomic.Int32 // 收到多少条 set_voice
}

func newFakeSidecar(t *testing.T) *fakeSidecar {
	t.Helper()
	f := &fakeSidecar{}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer c.CloseNow()
		ctx := r.Context()
		emitVoice := func(sid int, speed float64) {
			b, _ := json.Marshal(map[string]any{
				"type": "voice", "speakers": 187, "speaker_id": sid, "speed": speed,
			})
			_ = c.Write(ctx, websocket.MessageText, b)
		}
		emitVoice(0, 1) // 真 sidecar 连上就主动上报一次
		for {
			_, data, err := c.Read(ctx)
			if err != nil {
				return
			}
			var m map[string]any
			if json.Unmarshal(data, &m) != nil {
				continue
			}
			if m["type"] == "set_voice" {
				f.setVoice.Add(1)
				sid := 0
				if v, ok := m["speaker_id"].(float64); ok {
					sid = int(v)
				}
				emitVoice(sid, 1) // ← 回执：环的另一半
			}
		}
	}))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeSidecar) wsURL() string { return "ws" + f.srv.URL[len("http"):] }

// TestDesiredVoicePushedOncePerConnection 锁住一个真出过事的回归：
//
// 曾把"下发已保存音色"挂在 OnStateChange 上。但 sidecar 收到 set_voice 会回一条 voice 上报，
// 上报又触发 notify → 回调再发 set_voice → 无限乒乓。实测炸出 61 万次往返、86MB 日志，
// 每次 notify 还广播一次 status，风暴把浏览器打成"已断开·重连中"。
//
// 正确做法：下发时机是【连接建立】，不是【状态变化】——后者会被自己的结果再次触发。
// 故一条连接上，set_voice 必须恰好出现一次。
func TestDesiredVoicePushedOncePerConnection(t *testing.T) {
	f := newFakeSidecar(t)
	sc := NewSidecar(f.wsURL(), slog.New(slog.NewTextHandler(discard{}, nil)))
	sc.SetDesiredVoice(42, 1.0)

	// 模拟 main.go 的回调：只广播状态。若有人又在这里发命令，环会立刻炸出来。
	var notifies atomic.Int32
	sc.OnStateChange(func() { notifies.Add(1); _ = sc.Status() })

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := sc.Start(ctx); err != nil {
		t.Fatal(err)
	}
	defer sc.Close()

	// 给足时间：若存在乒乓，这 1.5 秒足以打出成千上万次。
	time.Sleep(1500 * time.Millisecond)

	if got := f.setVoice.Load(); got != 1 {
		t.Fatalf("一条连接上 set_voice 应恰好下发 1 次，实得 %d 次（>1 = 反馈环回来了）", got)
	}
	if got := sc.Status().Voice.SpeakerID; got != 42 {
		t.Fatalf("期望音色被下发为 42，实得 %d", got)
	}
	if got := sc.Status().Voice.Speakers; got != 187 {
		t.Fatalf("期望音色总数来自 sidecar 上报=187，实得 %d", got)
	}
}

// TestNoDesiredVoiceMeansNoPush 未配置音色时不该下发任何 set_voice——
// 沿用 sidecar 自己 config.json 里的默认值。
func TestNoDesiredVoiceMeansNoPush(t *testing.T) {
	f := newFakeSidecar(t)
	sc := NewSidecar(f.wsURL(), slog.New(slog.NewTextHandler(discard{}, nil)))
	// 故意不调 SetDesiredVoice

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := sc.Start(ctx); err != nil {
		t.Fatal(err)
	}
	defer sc.Close()
	time.Sleep(800 * time.Millisecond)

	if got := f.setVoice.Load(); got != 0 {
		t.Fatalf("未配置音色时不该下发 set_voice，实得 %d 次", got)
	}
}

type discard struct{}

func (discard) Write(p []byte) (int, error) { return len(p), nil }
