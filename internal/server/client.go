package server

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"time"

	"github.com/coder/websocket"

	"github.com/kingGang/ElectronStudio/internal/protocol"
)

// Client 表示一条 WebSocket 连接，封装其读/写泵与心跳。
//
// 并发模型：每个连接由两个 goroutine 协作——
//   - readLoop：持续读取消息，文本帧解码为命令后投递给 Server.deliver；
//   - writeLoop：从 send 队列取出消息写出，并按周期发送心跳 ping。
//
// 任一泵出错都会取消连接上下文，使另一个泵随之退出，最终关闭连接。
type Client struct {
	srv  *Server
	conn *websocket.Conn
	send chan outMsg
	log  *slog.Logger

	closeOnce sync.Once
}

// newClient 基于一条已握手的连接创建 Client。
func newClient(srv *Server, conn *websocket.Conn) *Client {
	return &Client{
		srv:  srv,
		conn: conn,
		send: make(chan outMsg, srv.opts.SendBuffer),
		log:  srv.opts.Logger,
	}
}

// enqueue 尝试把一条消息放入发送队列。返回 false 表示队列已满（消费过慢）。非阻塞。
func (c *Client) enqueue(msg outMsg) bool {
	select {
	case c.send <- msg:
		return true
	default:
		return false
	}
}

// dropOneDroppable 从发送队列里丢弃一条可丢弃消息（镜像帧），腾出空间给关键消息。
// 返回是否成功丢弃了一条。非阻塞：取出的若是关键消息则放回并停止。
func (c *Client) dropOneDroppable() bool {
	select {
	case m := <-c.send:
		if m.droppable {
			return true // 丢掉这条可丢弃帧，腾出了位置
		}
		// 取出的是关键消息：放回去（尽力），不丢
		select {
		case c.send <- m:
		default:
		}
		return false
	default:
		return false
	}
}

// run 启动读写泵并阻塞，直到连接结束。
func (c *Client) run(parent context.Context) {
	ctx, cancel := context.WithCancel(parent)
	defer cancel()
	// 连接收尾：确保底层资源释放。
	defer c.conn.CloseNow() //nolint:errcheck // 收尾阶段忽略关闭错误

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		c.writeLoop(ctx)
	}()

	// readLoop 在前台运行，返回即代表连接应当结束。
	c.readLoop(ctx)
	cancel()  // 通知 writeLoop 退出
	wg.Wait() // 等待 writeLoop 真正结束，避免 goroutine 泄漏
}

// readLoop 持续读取消息：
//   - 文本帧：解析为信封并投递给上层（仅处理客户端→服务端的命令）；
//   - 二进制帧：当前不处理入站二进制（屏幕镜像为单向下行），忽略之。
func (c *Client) readLoop(ctx context.Context) {
	for {
		typ, data, err := c.conn.Read(ctx)
		if err != nil {
			// 正常关闭或上下文取消时无需告警。
			if !isExpectedCloseErr(ctx, err) {
				c.log.Debug("读取结束", "err", err)
			}
			return
		}
		if typ != websocket.MessageText {
			continue // 入站二进制暂不支持
		}
		env, err := protocol.Decode(data)
		if err != nil {
			// 单条脏数据不应中断连接，记录后继续。
			c.log.Warn("入站消息解码失败", "err", err)
			continue
		}
		c.srv.deliver(Inbound{Client: c, Env: env})
	}
}

// writeLoop 从发送队列取消息写出，并按周期发送心跳。
func (c *Client) writeLoop(ctx context.Context) {
	ping := time.NewTicker(c.srv.opts.PingPeriod)
	defer ping.Stop()

	for {
		select {
		case <-ctx.Done():
			return

		case msg := <-c.send:
			if err := c.write(ctx, msg); err != nil {
				if !isExpectedCloseErr(ctx, err) {
					c.log.Debug("写入失败", "err", err)
				}
				return
			}

		case <-ping.C:
			// 心跳：Ping 会阻塞至收到 pong 或超时，用于探测死连接。
			pctx, cancel := context.WithTimeout(ctx, c.srv.opts.WriteTimeout)
			err := c.conn.Ping(pctx)
			cancel()
			if err != nil {
				if !isExpectedCloseErr(ctx, err) {
					c.log.Debug("心跳失败，断开连接", "err", err)
				}
				return
			}
		}
	}
}

// write 执行一次带超时的写操作。
func (c *Client) write(ctx context.Context, msg outMsg) error {
	wctx, cancel := context.WithTimeout(ctx, c.srv.opts.WriteTimeout)
	defer cancel()
	return c.conn.Write(wctx, msg.typ, msg.data)
}

// Send 向【该客户端】发送一个事件（定向回包，区别于 Server.Broadcast 的全员广播）。
// 非阻塞：队列满返回 error，由调用方决定如何处理。
func (c *Client) Send(p protocol.Payload) error {
	data, err := protocol.Encode(p)
	if err != nil {
		return err
	}
	if !c.enqueue(outMsg{typ: websocket.MessageText, data: data}) {
		return errors.New("server: 客户端发送队列已满")
	}
	return nil
}

// close 主动关闭连接（只生效一次）。
func (c *Client) close(code websocket.StatusCode, reason string) {
	c.closeOnce.Do(func() {
		_ = c.conn.Close(code, reason)
	})
}

// isExpectedCloseErr 判断错误是否为"预期内"的连接结束（上下文取消或正常关闭），
// 以避免把正常断开当作异常刷日志。
func isExpectedCloseErr(ctx context.Context, err error) bool {
	if ctx.Err() != nil {
		return true
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return true
	}
	switch websocket.CloseStatus(err) {
	case websocket.StatusNormalClosure, websocket.StatusGoingAway:
		return true
	}
	return false
}
