package trace

import "sync"

// MemoryStore 是单次 CLI 运行内的 trace 存储。
// ponytail: 单次 CLI 只需内存 trace；出现第二种存储实现时再引入接口。
type MemoryStore struct {
	mu      sync.Mutex
	entries []Entry
}

// NewMemoryStoreWithEntries 用已有 trace 初始化内存 store，主要用于 resume。
// 初始 entries 会被复制，避免调用方后续修改切片影响 store 内部状态。
func NewMemoryStoreWithEntries(entries []Entry) *MemoryStore {
	copied := make([]Entry, len(entries))
	copy(copied, entries)
	return &MemoryStore{entries: copied}
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
