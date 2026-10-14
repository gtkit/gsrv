package limiter

import "errors"

// Inflight 是非排队的并发 gate；一个实例可由 unary/stream 跨连接共用。
type Inflight struct{ slots chan struct{} }

// NewInflight 创建正容量 gate，不启动后台任务。
func NewInflight(capacity int) (*Inflight, error) {
	if capacity <= 0 {
		return nil, errors.New("limiter: inflight capacity must be positive")
	}
	return &Inflight{slots: make(chan struct{}, capacity)}, nil
}

// Acquire 尝试入场，满容量立即返回 false；成功后调用方必须 Release。
func (g *Inflight) Acquire() bool {
	select {
	case g.slots <- struct{}{}:
		return true
	default:
		return false
	}
}

// Release 释放一个已获槽位。
func (g *Inflight) Release() { <-g.slots }
