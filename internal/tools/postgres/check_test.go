package postgres

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

func TestPostgresCheckReportsMissingTables(t *testing.T) {
	runner := &fakeSQLRunner{
		tables: map[string]bool{
			"users": false,
		},
	}
	tool := &CheckTool{open: func(string) (sqlRunner, error) {
		return runner, nil
	}}

	observation, err := tool.Run(context.Background(), mustCheckArgs(t, CheckArgs{
		DSN:    "postgres://app:secret@localhost:5432/chat_proj?sslmode=disable",
		Tables: []string{"users"},
	}))
	if err != nil {
		t.Fatalf("run postgres check: %v", err)
	}

	if observation.Tool != CheckName {
		t.Fatalf("tool = %q, want %q", observation.Tool, CheckName)
	}
	if observation.Data["sql_ping_ok"] != true {
		t.Fatalf("sql_ping_ok = %#v, want true", observation.Data["sql_ping_ok"])
	}
	if observation.Data["database"] != "chat_proj" {
		t.Fatalf("database = %#v, want chat_proj", observation.Data["database"])
	}
	if !strings.Contains(observation.Summary, "db=chat_proj") || !strings.Contains(observation.Summary, "PostgreSQL 16.9") {
		t.Fatalf("summary = %q, want identity fingerprint", observation.Summary)
	}
	missing := observation.Data["missing_tables"].([]string)
	if len(missing) != 1 || missing[0] != "users" {
		t.Fatalf("missing tables = %#v, want users", missing)
	}
	if !strings.Contains(observation.Summary, "missing tables: users") {
		t.Fatalf("summary = %q, want missing users", observation.Summary)
	}
}

func TestPostgresCheckReturnsPingError(t *testing.T) {
	tool := &CheckTool{open: func(string) (sqlRunner, error) {
		return &fakeSQLRunner{pingErr: errors.New("password authentication failed")}, nil
	}}

	_, err := tool.Run(context.Background(), mustCheckArgs(t, CheckArgs{
		DSN: "postgres://app:bad@localhost:5432/chat_proj?sslmode=disable",
	}))
	if err == nil {
		t.Fatal("run succeeded, want ping error")
	}
	if !strings.Contains(err.Error(), "sql ping postgres") {
		t.Fatalf("error = %q, want sql ping postgres", err.Error())
	}
}

func mustCheckArgs(t *testing.T, args CheckArgs) json.RawMessage {
	t.Helper()
	data, err := json.Marshal(args)
	if err != nil {
		t.Fatalf("marshal args: %v", err)
	}
	return data
}

type fakeSQLRunner struct {
	pingErr error
	tables  map[string]bool
}

func (r *fakeSQLRunner) Ping(context.Context) error {
	return r.pingErr
}

func (r *fakeSQLRunner) Identity(context.Context) (identity, error) {
	return identity{
		Database:   "chat_proj",
		Version:    "PostgreSQL 16.9 on x86_64-pc-linux-musl, compiled by gcc",
		ServerAddr: "172.21.0.2",
	}, nil
}

func (r *fakeSQLRunner) TableExists(_ context.Context, table string) (bool, error) {
	return r.tables[table], nil
}

func (r *fakeSQLRunner) Close() error {
	return nil
}
