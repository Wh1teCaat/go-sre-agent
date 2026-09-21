package postgres

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	_ "github.com/lib/pq"
	"github.com/y2/go-sre-agent/internal/schema"
	"github.com/y2/go-sre-agent/internal/tools"
)

const CheckName = "postgres_check"

type CheckArgs struct {
	DSN    string   `json:"dsn"`
	Tables []string `json:"tables,omitempty"`
}

type CheckTool struct {
	// open 默认走 database/sql；测试在包内替换为 fake runner。
	open func(string) (sqlRunner, error)
}

type sqlRunner interface {
	Ping(context.Context) error
	Identity(context.Context) (identity, error)
	TableExists(context.Context, string) (bool, error)
	Close() error
}

// identity 是实例身份指纹，用于识别"端口上不是预期实例"的冒名场景，
// 例如宿主机口被另一个同名库的 PostgreSQL 占据。
type identity struct {
	Database   string
	Version    string
	ServerAddr string
}

func NewCheck() *CheckTool {
	return &CheckTool{open: openSQLRunner}
}

func (t *CheckTool) Spec() tools.ToolSpec {
	return tools.ToolSpec{
		Name:        CheckName,
		Description: "Authenticate to PostgreSQL, run a SQL ping, and optionally check table existence.",
		Schema: tools.ToolSchema{
			Properties: map[string]tools.ArgSpec{
				"dsn":    {Type: "string", Required: true, Description: "PostgreSQL connection string."},
				"tables": {Type: "array", Description: "Optional table names to check, for example users."},
			},
		},
	}
}

func (t *CheckTool) Run(ctx context.Context, rawArgs json.RawMessage) (schema.Observation, error) {
	var args CheckArgs
	if err := json.Unmarshal(rawArgs, &args); err != nil {
		return schema.Observation{}, fmt.Errorf("decode postgres_check args: %w", err)
	}
	if strings.TrimSpace(args.DSN) == "" {
		return schema.Observation{}, fmt.Errorf("postgres_check requires dsn")
	}

	startedAt := time.Now()
	db, err := t.open(args.DSN)
	if err != nil {
		return schema.Observation{}, fmt.Errorf("open postgres sql connection: %w", err)
	}
	defer db.Close()

	if err := db.Ping(ctx); err != nil {
		return schema.Observation{}, fmt.Errorf("sql ping postgres: %w", err)
	}
	who, err := db.Identity(ctx)
	if err != nil {
		return schema.Observation{}, fmt.Errorf("query postgres identity: %w", err)
	}

	checked := make([]string, 0, len(args.Tables))
	missing := []string{}
	for _, table := range args.Tables {
		table = strings.TrimSpace(table)
		if table == "" {
			continue
		}
		exists, err := db.TableExists(ctx, table)
		if err != nil {
			return schema.Observation{}, fmt.Errorf("check postgres table %q: %w", table, err)
		}
		checked = append(checked, table)
		if !exists {
			missing = append(missing, table)
		}
	}

	latencyMS := time.Since(startedAt).Milliseconds()
	summary := fmt.Sprintf("PostgreSQL SQL ping succeeded in %dms (db=%s, server=%s)", latencyMS, who.Database, versionPreview(who.Version))
	if len(checked) > 0 && len(missing) == 0 {
		summary += fmt.Sprintf("; tables exist: %s", strings.Join(checked, ", "))
	}
	if len(missing) > 0 {
		summary += fmt.Sprintf("; missing tables: %s", strings.Join(missing, ", "))
	}

	return schema.Observation{
		Tool:    CheckName,
		Summary: summary,
		Data: map[string]any{
			"sql_ping_ok":    true,
			"database":       who.Database,
			"server_version": who.Version,
			"server_addr":    who.ServerAddr,
			"checked_tables": checked,
			"missing_tables": missing,
			"latency_ms":     latencyMS,
		},
	}, nil
}

// versionPreview 截取 version() 的前几段：构建平台信息（Linux/Windows、编译器）
// 是判断"连到的是否为预期容器实例"的关键证据，但完整串太长不适合放 summary。
func versionPreview(version string) string {
	fields := strings.Fields(version)
	if len(fields) > 6 {
		fields = fields[:6]
	}
	return strings.Join(fields, " ")
}

type databaseSQLRunner struct {
	db *sql.DB
}

func openSQLRunner(dsn string) (sqlRunner, error) {
	db, err := sql.Open("postgres", dsn)
	if err != nil {
		return nil, err
	}
	return databaseSQLRunner{db: db}, nil
}

func (r databaseSQLRunner) Ping(ctx context.Context) error {
	return r.db.PingContext(ctx)
}

func (r databaseSQLRunner) Identity(ctx context.Context) (identity, error) {
	var who identity
	err := r.db.QueryRowContext(ctx,
		"select current_database(), version(), coalesce(inet_server_addr()::text, '')").
		Scan(&who.Database, &who.Version, &who.ServerAddr)
	return who, err
}

func (r databaseSQLRunner) TableExists(ctx context.Context, table string) (bool, error) {
	var name sql.NullString
	if err := r.db.QueryRowContext(ctx, "select to_regclass($1)", qualifiedTable(table)).Scan(&name); err != nil {
		return false, err
	}
	return name.Valid, nil
}

func (r databaseSQLRunner) Close() error {
	return r.db.Close()
}

func qualifiedTable(table string) string {
	if strings.Contains(table, ".") {
		return table
	}
	return "public." + table
}
