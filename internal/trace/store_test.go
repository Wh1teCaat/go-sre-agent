package trace

import (
	"testing"
	"time"

	"github.com/y2/go-sre-agent/internal/schema"
)

func TestMemoryStoreAppendsAndListsEntries(t *testing.T) {
	store := new(MemoryStore)

	entry := Entry{
		Step:           1,
		ThoughtSummary: "check backend health",
		ToolName:       "http_check",
		Args:           map[string]any{"url": "http://localhost:8080/health"},
		Result:         schema.Observation{Tool: "http_check", Summary: "200 OK"},
		Duration:       10 * time.Millisecond,
	}

	store.Append(entry)

	entries := store.List()
	if len(entries) != 1 {
		t.Fatalf("len(entries) = %d, want 1", len(entries))
	}
	if entries[0].Step != 1 {
		t.Fatalf("step = %d, want 1", entries[0].Step)
	}
	if entries[0].ToolName != "http_check" {
		t.Fatalf("tool = %q, want http_check", entries[0].ToolName)
	}
}

func TestMemoryStoreListReturnsCopy(t *testing.T) {
	store := new(MemoryStore)
	store.Append(Entry{Step: 1, ToolName: "log_read"})

	entries := store.List()
	entries[0].ToolName = "mutated"

	again := store.List()
	if again[0].ToolName != "log_read" {
		t.Fatalf("store was mutated through returned slice: got %q", again[0].ToolName)
	}
}

func TestNewMemoryStoreWithEntriesCopiesInitialEntries(t *testing.T) {
	initial := []Entry{{Step: 1, ToolName: "http_check"}}
	store := NewMemoryStoreWithEntries(initial)
	initial[0].ToolName = "mutated"

	entries := store.List()
	if len(entries) != 1 {
		t.Fatalf("len(entries) = %d, want 1", len(entries))
	}
	if entries[0].ToolName != "http_check" {
		t.Fatalf("tool = %q, want http_check", entries[0].ToolName)
	}
}
