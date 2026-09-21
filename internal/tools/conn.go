package tools

import (
	"context"
	"net"
	"time"
)

// ApplyConnDeadline 把 ctx 的 deadline 应用到连接；无 deadline 时用 5 秒兜底，
// 避免网络探测类工具在对端建连后不响应时无限挂起。
func ApplyConnDeadline(ctx context.Context, conn net.Conn) {
	deadline, ok := ctx.Deadline()
	if !ok {
		deadline = time.Now().Add(5 * time.Second)
	}
	_ = conn.SetDeadline(deadline)
}
