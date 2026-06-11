package server

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/kingGang/ElectronStudio/internal/protocol"
)

// startTestServer 启动一个仅供测试的 Server 与 httptest HTTP 服务，
// 返回 ws:// 拨号地址与清理函数。
func startTestServer(t *testing.T) (wsURL string, srv *Server, cleanup func()) {
	t.Helper()

	srv = New(Options{})
	ctx, cancel := context.WithCancel(context.Background())
	go srv.Run(ctx)

	httpSrv := httptest.NewServer(srv.Handler())
	wsURL = "ws" + strings.TrimPrefix(httpSrv.URL, "http") + "/ws"

	cleanup = func() {
		cancel()
		httpSrv.Close()
	}
	return wsURL, srv, cleanup
}

// dial 建立一条到测试服务的客户端连接。
func dial(t *testing.T, wsURL string) *websocket.Conn {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	conn, _, err := websocket.Dial(ctx, wsURL, nil)
	if err != nil {
		t.Fatalf("拨号失败: %v", err)
	}
	return conn
}

// waitClientCount 轮询等待连接数达到期望值（连接注册是异步的）。
func waitClientCount(t *testing.T, srv *Server, want int) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if srv.ClientCount() == want {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("连接数未达期望: 期望 %d 实际 %d", want, srv.ClientCount())
}

// TestBroadcast 验证：服务端广播的事件能被客户端收到并正确解码。
func TestBroadcast(t *testing.T) {
	wsURL, srv, cleanup := startTestServer(t)
	defer cleanup()

	conn := dial(t, wsURL)
	defer conn.Close(websocket.StatusNormalClosure, "")
	waitClientCount(t, srv, 1)

	srv.Broadcast(VoiceStateEventOf(protocol.VoiceListening))

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	typ, data, err := conn.Read(ctx)
	if err != nil {
		t.Fatalf("读取失败: %v", err)
	}
	if typ != websocket.MessageText {
		t.Fatalf("期望文本帧, 实际 %v", typ)
	}
	env, err := protocol.Decode(data)
	if err != nil {
		t.Fatalf("解码失败: %v", err)
	}
	ev, err := protocol.As[protocol.VoiceStateEvent](env)
	if err != nil {
		t.Fatalf("取负载失败: %v", err)
	}
	if ev.State != protocol.VoiceListening {
		t.Fatalf("状态错误: 期望 %q 实际 %q", protocol.VoiceListening, ev.State)
	}
}

// TestInbound 验证：客户端发送的命令能从 Server.Inbound() 取到并正确解码。
func TestInbound(t *testing.T) {
	wsURL, srv, cleanup := startTestServer(t)
	defer cleanup()

	conn := dial(t, wsURL)
	defer conn.Close(websocket.StatusNormalClosure, "")
	waitClientCount(t, srv, 1)

	cmd := protocol.SendTextCommand{Text: "帮我开台灯"}
	data, err := protocol.Encode(cmd)
	if err != nil {
		t.Fatalf("编码失败: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := conn.Write(ctx, websocket.MessageText, data); err != nil {
		t.Fatalf("写入失败: %v", err)
	}

	select {
	case in := <-srv.Inbound():
		got, err := protocol.As[protocol.SendTextCommand](in.Env)
		if err != nil {
			t.Fatalf("取负载失败: %v", err)
		}
		if got.Text != cmd.Text {
			t.Fatalf("内容不一致: 期望 %q 实际 %q", cmd.Text, got.Text)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("超时未收到入站命令")
	}
}

// TestBroadcastFrame 验证：屏幕镜像帧以二进制帧广播，客户端可解析回原始数据。
func TestBroadcastFrame(t *testing.T) {
	wsURL, srv, cleanup := startTestServer(t)
	defer cleanup()

	conn := dial(t, wsURL)
	defer conn.Close(websocket.StatusNormalClosure, "")
	waitClientCount(t, srv, 1)

	const w, h = 2, 2
	hdr := protocol.FrameHeader{Width: w, Height: h, Format: protocol.PixelRGB888, Seq: 1}
	pixels := make([]byte, w*h*3)
	frame, err := protocol.EncodeFrame(hdr, pixels)
	if err != nil {
		t.Fatalf("打包帧失败: %v", err)
	}
	srv.BroadcastFrame(frame)

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	typ, data, err := conn.Read(ctx)
	if err != nil {
		t.Fatalf("读取失败: %v", err)
	}
	if typ != websocket.MessageBinary {
		t.Fatalf("期望二进制帧, 实际 %v", typ)
	}
	gotHdr, _, err := protocol.DecodeFrame(data)
	if err != nil {
		t.Fatalf("解析帧失败: %v", err)
	}
	if gotHdr != hdr {
		t.Fatalf("帧头不一致: 期望 %+v 实际 %+v", hdr, gotHdr)
	}
}

// TestDisconnectUpdatesCount 验证：客户端断开后连接数回落。
func TestDisconnectUpdatesCount(t *testing.T) {
	wsURL, srv, cleanup := startTestServer(t)
	defer cleanup()

	conn := dial(t, wsURL)
	waitClientCount(t, srv, 1)

	conn.Close(websocket.StatusNormalClosure, "bye")
	waitClientCount(t, srv, 0)
}

// VoiceStateEventOf 是测试辅助：构造一个 VoiceStateEvent。
func VoiceStateEventOf(s protocol.VoiceState) protocol.VoiceStateEvent {
	return protocol.VoiceStateEvent{State: s}
}

// 确保 http 包被引用（保留以便后续扩展静态资源相关测试）。
var _ = http.StatusOK
