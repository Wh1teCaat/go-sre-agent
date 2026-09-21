package trace

// MemoryStore 是单次 CLI 运行内的 trace 存储。
// 单次 CLI 只需内存 trace；出现第二种存储实现时再引入接口。
type MemoryStore struct {
	entries []Entry
}

// NewMemoryStoreWithEntries 用已有 trace 初始化内存 store，主要用于 resume。
// 参数: entries 为不再由调用方使用的初始记录；返回: 持有这些记录的内存 store。
func NewMemoryStoreWithEntries(entries []Entry) *MemoryStore {
	return &MemoryStore{entries: entries}
}

func (s *MemoryStore) Append(entry Entry) {
	s.entries = append(s.entries, entry)
}

func (s *MemoryStore) List() []Entry {
	return s.entries
}
