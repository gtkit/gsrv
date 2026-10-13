package auth

import (
	"container/heap"
	"context"
	"errors"
	"sync"
	"time"
)

// NonceStore 原子消费尚未到期的 nonce；false 表示重复，error 表示不可安全判断。
// expiresAt 是请求时间戳加容忍窗口；实现不得在到期前驱逐有效 nonce。
type NonceStore interface {
	Use(context.Context, string, time.Time) (bool, error)
}

// MemoryNonceStore 是有容量上限的进程内去重存储；不跨重启或副本共享。
type MemoryNonceStore struct {
	mu       sync.Mutex
	capacity int
	items    map[string]time.Time
	expiry   expiryHeap
}

// NewMemoryNonceStore 创建去重存储，capacity 必须为正值；不启动后台 goroutine。
func NewMemoryNonceStore(capacity int) (*MemoryNonceStore, error) {
	if capacity <= 0 {
		return nil, errors.New("auth: nonce capacity must be positive")
	}
	return &MemoryNonceStore{capacity: capacity, items: make(map[string]time.Time)}, nil
}

// Use 检查并消费 nonce；并发同 key 至多一项成功，满容量时拒绝新 nonce。
func (s *MemoryNonceStore) Use(ctx context.Context, key string, expiresAt time.Time) (bool, error) {
	if err := ctx.Err(); err != nil {
		return false, err
	}
	now := time.Now()
	if len(key) > 256 || key == "" || !expiresAt.After(now) {
		return false, errors.New("auth: invalid nonce retention")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for len(s.expiry) > 0 && !s.expiry[0].at.After(now) {
		old := heap.Pop(&s.expiry).(expiryEntry)
		delete(s.items, old.key)
	}
	if _, exists := s.items[key]; exists {
		return false, nil
	}
	if len(s.items) >= s.capacity {
		return false, errors.New("auth: nonce capacity exhausted")
	}
	s.items[key] = expiresAt
	heap.Push(&s.expiry, expiryEntry{key: key, at: expiresAt})
	return true, nil
}

type expiryEntry struct {
	key string
	at  time.Time
}
type expiryHeap []expiryEntry

func (h expiryHeap) Len() int           { return len(h) }
func (h expiryHeap) Less(i, j int) bool { return h[i].at.Before(h[j].at) }
func (h expiryHeap) Swap(i, j int)      { h[i], h[j] = h[j], h[i] }
func (h *expiryHeap) Push(value any)    { *h = append(*h, value.(expiryEntry)) }
func (h *expiryHeap) Pop() any {
	old := *h
	value := old[len(old)-1]
	old[len(old)-1] = expiryEntry{}
	*h = old[:len(old)-1]
	return value
}
