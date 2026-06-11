// Package server 把 internal/protocol 定义的消息契约接到真实的 WebSocket 服务上。
//
// 职责边界（刻意保持"哑"，不含任何业务逻辑）：
//   - 接受 WebSocket 连接，管理连接生命周期（注册/注销/心跳/优雅关闭）；
//   - 向所有客户端【广播】事件（protocol.Payload）与屏幕镜像帧（二进制）；
//   - 把客户端发来的【命令】解码后投递到 Inbound() 通道，交由上层
//     （fsm / llm / choreography 等）消费。
//
// 这样语音、对话、机器人控制等业务模块只依赖本包的 Broadcast / Inbound，
// 而无需关心 WebSocket 细节，便于测试与替换。
package server

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"net/http"
	"time"

	"github.com/coder/websocket"

	"github.com/kingGang/ElectronStudio/internal/protocol"
)

// 默认参数。可通过 Options 覆盖。
const (
	defaultSendBuffer      = 64               // 每客户端发送队列容量
	defaultInboundBuffer   = 128              // 全局入站命令队列容量
	defaultPingPeriod      = 30 * time.Second // 心跳间隔
	defaultWriteTimeout    = 10 * time.Second // 单次写超时
	defaultMaxMessageBytes = 1 << 20          // 单条入站消息上限(1MiB)，防御性限制
)

// Inbound 表示一条来自某客户端的入站命令。
type Inbound struct {
	Client *Client            // 来源连接，可用于定向回包
	Env    protocol.Envelope  // 已解码的信封；按 Env.Type 用 protocol.As 取负载
}

// Options 配置一个 Server。所有字段均可留零值，将回退到默认值。
type Options struct {
	// StaticFS 为前端静态资源（通常由 go:embed 提供）。为 nil 时不提供静态文件。
	StaticFS fs.FS
	// SendBuffer 为每个客户端的发送队列容量。队列满意味着客户端消费过慢，
	// 该连接会被主动断开，以免拖垮整个广播。
	SendBuffer int
	// InboundBuffer 为全局入站命令队列容量。
	InboundBuffer int
	// PingPeriod 为心跳间隔。
	PingPeriod time.Duration
	// WriteTimeout 为单次写操作的超时时间。
	WriteTimeout time.Duration
	// MaxMessageBytes 为单条入站消息的字节上限。
	MaxMessageBytes int64
	// OriginPatterns 为允许的来源模式（同 coder/websocket）。为空表示仅允许同源。
	OriginPatterns []string
	// Logger 为日志器。为 nil 时使用 slog 默认 logger。
	Logger *slog.Logger
	// OnConnect 在一个新客户端注册成功后被调用（可选），常用于向其推送初始状态快照。
	// 回调在连接的接管 goroutine 中同步执行，应尽量轻量、避免阻塞。
	OnConnect func(c *Client)
}

// withDefaults 返回填充了默认值的 Options 副本，避免在各处重复判空。
func (o Options) withDefaults() Options {
	if o.SendBuffer <= 0 {
		o.SendBuffer = defaultSendBuffer
	}
	if o.InboundBuffer <= 0 {
		o.InboundBuffer = defaultInboundBuffer
	}
	if o.PingPeriod <= 0 {
		o.PingPeriod = defaultPingPeriod
	}
	if o.WriteTimeout <= 0 {
		o.WriteTimeout = defaultWriteTimeout
	}
	if o.MaxMessageBytes <= 0 {
		o.MaxMessageBytes = defaultMaxMessageBytes
	}
	if o.Logger == nil {
		o.Logger = slog.Default()
	}
	return o
}

// Server 是 WebSocket 服务的对外门面。
type Server struct {
	opts    Options
	hub     *hub
	inbound chan Inbound
	log     *slog.Logger
}

// New 创建一个 Server。需调用 Run（或 Serve）启动其内部事件循环后才会工作。
func New(opts Options) *Server {
	opts = opts.withDefaults()
	s := &Server{
		opts:    opts,
		inbound: make(chan Inbound, opts.InboundBuffer),
		log:     opts.Logger,
	}
	s.hub = newHub(opts.Logger)
	return s
}

// Run 启动内部连接管理循环，阻塞直到 ctx 被取消。
// 通常在独立 goroutine 中调用，或直接使用 Serve。
func (s *Server) Run(ctx context.Context) {
	s.hub.run(ctx)
}

// Handler 返回 HTTP 路由，包含 WebSocket 端点 /ws 及（可选）前端静态资源。
// 便于调用方将其挂载到自有的 http.Server 或与其他路由组合。
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/ws", s.handleWS)
	if s.opts.StaticFS != nil {
		mux.Handle("/", http.FileServer(http.FS(s.opts.StaticFS)))
	}
	return mux
}

// Serve 是便捷方法：同时启动连接管理循环与 HTTP 服务，并在 ctx 取消时优雅关闭。
func (s *Server) Serve(ctx context.Context, addr string) error {
	// 连接管理循环随 ctx 生命周期运行。
	go s.Run(ctx)

	httpSrv := &http.Server{
		Addr:    addr,
		Handler: s.Handler(),
	}

	// 监听 ctx 取消并触发优雅关闭。
	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = httpSrv.Shutdown(shutdownCtx)
	}()

	s.log.Info("WebSocket 服务启动", "addr", addr)
	if err := httpSrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return fmt.Errorf("server: HTTP 服务异常: %w", err)
	}
	return nil
}

// Broadcast 向所有已连接客户端发送一个事件。编码只进行一次。
// 该调用是非阻塞的：若个别客户端队列已满，仅该客户端会被断开，不影响其他连接。
func (s *Server) Broadcast(p protocol.Payload) {
	data, err := protocol.Encode(p)
	if err != nil {
		s.log.Error("广播编码失败", "type", p.Type(), "err", err)
		return
	}
	s.hub.broadcast(outMsg{typ: websocket.MessageText, data: data})
}

// BroadcastFrame 向所有客户端发送一帧屏幕镜像数据（二进制帧）。
// frame 应为 protocol.EncodeFrame 的产物。
func (s *Server) BroadcastFrame(frame []byte) {
	s.hub.broadcast(outMsg{typ: websocket.MessageBinary, data: frame})
}

// Inbound 返回入站命令通道，供上层业务消费。
// 调用方必须持续读取本通道，否则队列满后入站命令将被丢弃（并记录告警）。
func (s *Server) Inbound() <-chan Inbound {
	return s.inbound
}

// ClientCount 返回当前连接数（主要用于监控/测试）。
func (s *Server) ClientCount() int {
	return s.hub.count()
}

// handleWS 升级 HTTP 连接为 WebSocket 并接管其生命周期。
func (s *Server) handleWS(w http.ResponseWriter, r *http.Request) {
	conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{
		OriginPatterns: s.opts.OriginPatterns,
	})
	if err != nil {
		s.log.Warn("WebSocket 握手失败", "remote", r.RemoteAddr, "err", err)
		return
	}
	conn.SetReadLimit(s.opts.MaxMessageBytes)

	c := newClient(s, conn)
	s.hub.add(c)
	s.log.Debug("客户端已连接", "remote", r.RemoteAddr, "total", s.hub.count())

	// 通知上层有新连接（如推送初始状态快照）。
	if s.opts.OnConnect != nil {
		s.opts.OnConnect(c)
	}

	// run 阻塞至连接结束；结束后从 hub 注销。
	// 使用请求上下文，使服务关闭时连接随之收敛。
	c.run(r.Context())
	s.hub.remove(c)
	s.log.Debug("客户端已断开", "remote", r.RemoteAddr, "total", s.hub.count())
}

// deliver 由 Client 在收到合法入站命令时调用，将其投递到 Inbound 通道。
// 非阻塞：若通道已满则丢弃并告警，避免拖慢读取泵导致连接假死。
func (s *Server) deliver(in Inbound) {
	select {
	case s.inbound <- in:
	default:
		s.log.Warn("入站队列已满，丢弃命令", "type", in.Env.Type)
	}
}
