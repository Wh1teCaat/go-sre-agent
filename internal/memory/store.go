package memory

import (
	"strings"
	"time"
)

const (
	// DefaultDir 是跨会话知识文件的固定根目录。
	DefaultDir = "memories"

	memoryGeneration = "go-sre-agent/memory-v1"
	rolloutDirName   = "rollout_summaries"
	rawFileName      = "raw_memories.md"
	indexFileName    = "MEMORY.md"
	summaryFileName  = "memory_summary.md"
	lockFileName     = ".memory.lock"
	lockStaleAfter   = 5 * time.Minute
)

// CollectionStatus 表示一份复盘在跨会话检索中的生命周期状态。
type CollectionStatus string

const (
	CollectionActive      CollectionStatus = "active"
	CollectionInvalidated CollectionStatus = "invalidated"
	CollectionDeleted     CollectionStatus = "deleted"
)

// Store 管理 memories 根目录。写入操作通过进程间锁和单文件原子替换保护。
type Store struct {
	dir string
}

// Query 限制跨会话检索范围和上下文大小。Service、Environment 为空时表示不限定该
// 字段；正常 runtime 会始终传入两者，避免跨环境混入历史知识。
type Query struct {
	Service     string
	Environment string
	Goal        string
	MaxMatches  int
	MaxBytes    int
}

// Match 是一次检索命中的复盘摘要。Content 只包含适合模型阅读的有限历史材料。
type Match struct {
	RunID            string
	SessionID        string
	Service          string
	Environment      string
	Outcome          string
	ConclusionStatus string
	Keywords         []string
	Content          string
}

type rolloutDocument struct {
	Generation             string           `yaml:"generation"`
	Kind                   string           `yaml:"kind"`
	ContentDigest          string           `yaml:"content_digest"`
	RunID                  string           `yaml:"run_id"`
	SessionID              string           `yaml:"session_id,omitempty"`
	Service                string           `yaml:"service"`
	Environment            string           `yaml:"environment"`
	Outcome                string           `yaml:"outcome"`
	SourceConclusionStatus string           `yaml:"source_conclusion_status"`
	ConclusionStatus       string           `yaml:"conclusion_status"`
	ConclusionOverride     string           `yaml:"conclusion_override,omitempty"`
	CollectionStatus       CollectionStatus `yaml:"collection_status"`
	CorrectionNote         string           `yaml:"correction_note,omitempty"`
	InvalidationReason     string           `yaml:"invalidation_reason,omitempty"`
	SourceDigest           string           `yaml:"source_digest"`
	Keywords               []string         `yaml:"keywords"`
	Title                  string           `yaml:"title"`
	Goal                   string           `yaml:"goal"`
	Phenomenon             string           `yaml:"phenomenon"`
	KeyObservations        []string         `yaml:"key_observations,omitempty"`
	Unresolved             []string         `yaml:"unresolved,omitempty"`
	CandidateExperience    []string         `yaml:"candidate_experience,omitempty"`
	FailureLessons         []string         `yaml:"failure_lessons,omitempty"`
	EvidenceSources        []string         `yaml:"evidence_sources,omitempty"`
	UpdatedAt              time.Time        `yaml:"updated_at"`
}

type rawEntry struct {
	RunID            string           `yaml:"run_id"`
	Service          string           `yaml:"service"`
	Environment      string           `yaml:"environment"`
	Outcome          string           `yaml:"outcome"`
	ConclusionStatus string           `yaml:"conclusion_status"`
	CollectionStatus CollectionStatus `yaml:"collection_status"`
	Keywords         []string         `yaml:"keywords"`
	Experience       []string         `yaml:"candidate_experience,omitempty"`
	FailureLessons   []string         `yaml:"failure_lessons,omitempty"`
	Applicability    string           `yaml:"applicability"`
	Source           string           `yaml:"source"`
}

type rawDocument struct {
	Generation    string     `yaml:"generation"`
	Kind          string     `yaml:"kind"`
	ContentDigest string     `yaml:"content_digest"`
	Entries       []rawEntry `yaml:"entries"`
}

type topic struct {
	Subject     string   `yaml:"subject"`
	Service     string   `yaml:"service"`
	Environment string   `yaml:"environment"`
	Keywords    []string `yaml:"keywords"`
	Knowledge   []string `yaml:"knowledge,omitempty"`
	RunIDs      []string `yaml:"run_ids"`
}

type indexDocument struct {
	Generation    string  `yaml:"generation"`
	Kind          string  `yaml:"kind"`
	ContentDigest string  `yaml:"content_digest"`
	Topics        []topic `yaml:"topics"`
}

type summaryDocument struct {
	Generation    string  `yaml:"generation"`
	Kind          string  `yaml:"kind"`
	ContentDigest string  `yaml:"content_digest"`
	Topics        []topic `yaml:"topics"`
}

// NewStore 创建 memories 存储。空目录使用固定的 memories 根目录。
func NewStore(dir string) *Store {
	if strings.TrimSpace(dir) == "" {
		dir = DefaultDir
	}
	return &Store{dir: dir}
}

// UpdateForRun 根据已保存的终态 run 更新单次复盘并重建其余索引。返回 false 表示
// run 不具备收录条件，例如仍在运行、没有工具观察或只是空的模型失败。
