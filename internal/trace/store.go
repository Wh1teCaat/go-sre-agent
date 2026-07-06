package trace

import "sync"

type Store interface {
	Append(entry Entry)
	List() []Entry
}

// MemoryStore 是单次 CLI 运行内的 trace 存储。
// 当前项目不跨进程持久化 trace，报告生成完成后这份内存状态即可丢弃。
type MemoryStore struct {
	mu      sync.Mutex
	entries []Entry
}

func NewMemoryStore() *MemoryStore {
	return &MemoryStore{}
}

func (s *MemoryStore) Append(entry Entry) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.entries = append(s.entries, entry)
}

func (s *MemoryStore) List() []Entry {
	s.mu.Lock()
	defer s.mu.Unlock()

	// 返回副本，避免调用方修改底层 slice 破坏后续 evidence 校验或报告生成。
	entries := make([]Entry, len(s.entries))
	copy(entries, s.entries)
	return entries
}
