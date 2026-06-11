package server

import (
	"context"
	"log/slog"
	"sync"

	"github.com/coder/websocket"
)

// outMsg 是一条待发送的出站消息，携带 WebSocket 帧类型（文本/二进制）。
type outMsg struct {
	typ  websocket.MessageType
	data []byte
}

// hub 集中管理所有客户端连接，并负责广播。
//
// 设计：客户端集合由 hub 自己的 run goroutine 独占访问（通过 channel 通信），
// 从而避免对 map 加锁；count() 等只读查询则用一把轻量 RWMutex 维护一个计数副本，
// 以便在任意 goroutine 安全读取连接数。
type hub struct {
	registerCh   chan *Client
	unregisterCh chan *Client
	broadcastCh  chan outMsg

	clients map[*Client]struct{}

	mu sync.RWMutex // 仅保护 n
	n  int          // 当前连接数的可并发读取副本

	log *slog.Logger
}

// newHub 创建一个 hub。需调用 run 启动其事件循环。
func newHub(log *slog.Logger) *hub {
	return &hub{
		registerCh:   make(chan *Client),
		unregisterCh: make(chan *Client),
		broadcastCh:  make(chan outMsg, 64),
		clients:      make(map[*Client]struct{}),
		log:          log,
	}
}

// run 是 hub 的事件循环，独占 clients 集合，直到 ctx 取消。
func (h *hub) run(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			// 关闭所有连接并退出。
			for c := range h.clients {
				c.close(websocket.StatusGoingAway, "server shutting down")
			}
			h.clients = make(map[*Client]struct{})
			h.setCount(0)
			return

		case c := <-h.registerCh:
			h.clients[c] = struct{}{}
			h.setCount(len(h.clients))

		case c := <-h.unregisterCh:
			if _, ok := h.clients[c]; ok {
				delete(h.clients, c)
				h.setCount(len(h.clients))
			}

		case msg := <-h.broadcastCh:
			h.fanout(msg)
		}
	}
}

// fanout 把一条消息投递给所有客户端。
// 对消费过慢（发送队列已满）的客户端，直接断开，避免阻塞广播循环。
func (h *hub) fanout(msg outMsg) {
	for c := range h.clients {
		if !c.enqueue(msg) {
			h.log.Warn("客户端发送队列已满，断开慢连接")
			c.close(websocket.StatusPolicyViolation, "send buffer overflow")
			delete(h.clients, c)
			h.setCount(len(h.clients))
		}
	}
}

// add 注册一个客户端。
func (h *hub) add(c *Client) { h.registerCh <- c }

// remove 注销一个客户端。
func (h *hub) remove(c *Client) { h.unregisterCh <- c }

// broadcast 投递一条待广播消息（非阻塞地交给 run 循环）。
func (h *hub) broadcast(msg outMsg) {
	select {
	case h.broadcastCh <- msg:
	default:
		// 广播积压说明系统整体过载，丢弃本条以保护服务。
		h.log.Warn("广播队列已满，丢弃消息")
	}
}

// count 返回当前连接数（并发安全）。
func (h *hub) count() int {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return h.n
}

func (h *hub) setCount(n int) {
	h.mu.Lock()
	h.n = n
	h.mu.Unlock()
}
