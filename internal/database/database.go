package database

import (
	"database/sql"
	"strings"
	"time"

	log "github.com/sirupsen/logrus"
	_ "modernc.org/sqlite"
)

// ModelRoute 模型路由表结构
// FailoverGroupID > 0 时，该行是某个故障转移分组的端点，不再单独按模型对外提供
type ModelRoute struct {
	ID              int64     `json:"id"`
	Name            string    `json:"name"`
	Model           string    `json:"model"`
	APIUrl          string    `json:"api_url"`
	APIKey          string    `json:"api_key"`
	Format          string    `json:"format"`            // 格式类型 (openai, claude, gemini)
	FailoverGroupID int64     `json:"failover_group_id"` // 0 = 未分组
	Priority        int       `json:"priority"`          // 分组内优先级，越小越优先
	Enabled         bool      `json:"enabled"`
	CreatedAt       time.Time `json:"created_at"`
	UpdatedAt       time.Time `json:"updated_at"`
}

// FailoverGroup 故障转移分组：对外唯一 model id，组内端点按 priority 顺序尝试
type FailoverGroup struct {
	ID              int64     `json:"id"`
	Name            string    `json:"name"`
	Model           string    `json:"model"`
	CooldownSeconds int       `json:"cooldown_seconds"`
	Enabled         bool      `json:"enabled"`
	CreatedAt       time.Time `json:"created_at"`
	UpdatedAt       time.Time `json:"updated_at"`
}

// RequestLog 请求日志表结构
type RequestLog struct {
	ID             int64     `json:"id"`
	Model          string    `json:"model"`
	RouteID        int64     `json:"route_id"`
	RequestTokens  int       `json:"request_tokens"`
	ResponseTokens int       `json:"response_tokens"`
	TotalTokens    int       `json:"total_tokens"`
	Success        bool      `json:"success"`
	ErrorMessage   string    `json:"error_message"`
	CreatedAt      time.Time `json:"created_at"`
}

// HourlyStats 每小时统计表结构（压缩后的数据）
type HourlyStats struct {
	ID             int64  `json:"id"`
	Date           string `json:"date"`            // 日期 YYYY-MM-DD
	Hour           int    `json:"hour"`            // 小时 0-23
	Model          string `json:"model"`           // 模型名
	RequestCount   int    `json:"request_count"`   // 请求次数
	RequestTokens  int    `json:"request_tokens"`  // 输入 token
	ResponseTokens int    `json:"response_tokens"` // 输出 token
	TotalTokens    int    `json:"total_tokens"`    // 总 token
	SuccessCount   int    `json:"success_count"`   // 成功次数
	FailCount      int    `json:"fail_count"`      // 失败次数
}

// UsageSummary 用量汇总表结构
type UsageSummary struct {
	ID             int64  `json:"id"`
	PeriodType     string `json:"period_type"`     // week, year, total
	PeriodKey      string `json:"period_key"`      // 2026-W02, 2026, total
	RequestCount   int64  `json:"request_count"`   // 请求次数
	RequestTokens  int64  `json:"request_tokens"`  // 输入 token
	ResponseTokens int64  `json:"response_tokens"` // 输出 token
	TotalTokens    int64  `json:"total_tokens"`    // 总 token
	SuccessCount   int64  `json:"success_count"`   // 成功次数
	FailCount      int64  `json:"fail_count"`      // 失败次数
	UpdatedAt      string `json:"updated_at"`      // 更新时间
}

// buildDSN 构造 SQLite DSN
// WAL + busy_timeout 是多会话并发的关键：读写不互相阻塞，写冲突自动重试而不是直接 database is locked
func buildDSN(dbPath string) string {
	if strings.Contains(dbPath, "?") {
		return dbPath
	}
	return dbPath + "?_pragma=busy_timeout(10000)&_pragma=journal_mode(WAL)&_pragma=synchronous(NORMAL)"
}

func InitDB(dbPath string) (*sql.DB, error) {
	db, err := sql.Open("sqlite", buildDSN(dbPath))
	if err != nil {
		return nil, err
	}

	// 每个请求都会查路由 + 写请求日志，连接池必须允许并发
	db.SetMaxOpenConns(32)
	db.SetMaxIdleConns(16)

	if err := db.Ping(); err != nil {
		db.Close()
		return nil, err
	}

	// 创建表
	if err := createTables(db); err != nil {
		db.Close()
		return nil, err
	}

	// 执行数据库迁移
	if err := migrateDB(db); err != nil {
		log.Warnf("Database migration warning: %v", err)
	}

	log.Info("Database initialized successfully")
	return db, nil
}

func createTables(db *sql.DB) error {
	schema := `
	CREATE TABLE IF NOT EXISTS failover_groups (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		name TEXT NOT NULL,
		model TEXT NOT NULL,
		cooldown_seconds INTEGER DEFAULT 600,
		enabled INTEGER DEFAULT 1,
		created_at DATETIME DEFAULT CURRENT_TIMESTAMP,
		updated_at DATETIME DEFAULT CURRENT_TIMESTAMP
	);

	CREATE UNIQUE INDEX IF NOT EXISTS idx_failover_groups_model ON failover_groups(model);

	CREATE TABLE IF NOT EXISTS model_routes (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		name TEXT NOT NULL,
		model TEXT NOT NULL,
		api_url TEXT NOT NULL,
		api_key TEXT,
		format TEXT DEFAULT 'openai',
		failover_group_id INTEGER DEFAULT 0,
		priority INTEGER DEFAULT 0,
		enabled INTEGER DEFAULT 1,
		created_at DATETIME DEFAULT CURRENT_TIMESTAMP,
		updated_at DATETIME DEFAULT CURRENT_TIMESTAMP
	);

	CREATE INDEX IF NOT EXISTS idx_model_routes_model ON model_routes(model);
	CREATE INDEX IF NOT EXISTS idx_model_routes_enabled ON model_routes(enabled);
	CREATE INDEX IF NOT EXISTS idx_model_routes_group ON model_routes(failover_group_id, priority);

	CREATE TABLE IF NOT EXISTS request_logs (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		model TEXT NOT NULL,
		route_id INTEGER,
		request_tokens INTEGER DEFAULT 0,
		response_tokens INTEGER DEFAULT 0,
		total_tokens INTEGER DEFAULT 0,
		success INTEGER DEFAULT 1,
		error_message TEXT,
		created_at DATETIME DEFAULT CURRENT_TIMESTAMP,
		FOREIGN KEY (route_id) REFERENCES model_routes(id) ON DELETE SET NULL
	);

	CREATE INDEX IF NOT EXISTS idx_request_logs_model ON request_logs(model);
	CREATE INDEX IF NOT EXISTS idx_request_logs_route_id ON request_logs(route_id);
	CREATE INDEX IF NOT EXISTS idx_request_logs_created_at ON request_logs(created_at);
	CREATE INDEX IF NOT EXISTS idx_request_logs_success ON request_logs(success);

	-- 每小时统计表（压缩后的数据）
	CREATE TABLE IF NOT EXISTS hourly_stats (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		date TEXT NOT NULL,
		hour INTEGER NOT NULL,
		model TEXT NOT NULL,
		request_count INTEGER DEFAULT 0,
		request_tokens INTEGER DEFAULT 0,
		response_tokens INTEGER DEFAULT 0,
		total_tokens INTEGER DEFAULT 0,
		success_count INTEGER DEFAULT 0,
		fail_count INTEGER DEFAULT 0,
		UNIQUE(date, hour, model)
	);

	CREATE INDEX IF NOT EXISTS idx_hourly_stats_date ON hourly_stats(date);
	CREATE INDEX IF NOT EXISTS idx_hourly_stats_model ON hourly_stats(model);

	-- 用量汇总表（周/年/总用量）
	CREATE TABLE IF NOT EXISTS usage_summary (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		period_type TEXT NOT NULL,
		period_key TEXT NOT NULL,
		request_count INTEGER DEFAULT 0,
		request_tokens INTEGER DEFAULT 0,
		response_tokens INTEGER DEFAULT 0,
		total_tokens INTEGER DEFAULT 0,
		success_count INTEGER DEFAULT 0,
		fail_count INTEGER DEFAULT 0,
		updated_at DATETIME DEFAULT CURRENT_TIMESTAMP,
		UNIQUE(period_type, period_key)
	);

	CREATE INDEX IF NOT EXISTS idx_usage_summary_type ON usage_summary(period_type);
	CREATE INDEX IF NOT EXISTS idx_usage_summary_key ON usage_summary(period_key);
	`

	_, err := db.Exec(schema)
	return err
}

// migrateDB 执行数据库迁移，确保表结构是最新的
func migrateDB(db *sql.DB) error {
	db.Exec(`ALTER TABLE model_routes ADD COLUMN format TEXT DEFAULT 'openai'`)
	db.Exec(`ALTER TABLE model_routes ADD COLUMN failover_group_id INTEGER DEFAULT 0`)
	db.Exec(`ALTER TABLE model_routes ADD COLUMN priority INTEGER DEFAULT 0`)

	db.Exec(`ALTER TABLE hourly_stats ADD COLUMN success_count INTEGER DEFAULT 0`)
	db.Exec(`ALTER TABLE hourly_stats ADD COLUMN fail_count INTEGER DEFAULT 0`)

	db.Exec(`ALTER TABLE usage_summary ADD COLUMN success_count INTEGER DEFAULT 0`)
	db.Exec(`ALTER TABLE usage_summary ADD COLUMN fail_count INTEGER DEFAULT 0`)

	// 旧版本的 "group" 只是展示字段，这里升级为故障转移分组
	migrateLegacyGroups(db)

	log.Info("Database migration completed")
	return nil
}

// migrateLegacyGroups 将旧的展示用 group 字段升级为故障转移分组
// 旧分组内的路由成为该分组的端点，按 id 顺序排列优先级
func migrateLegacyGroups(db *sql.DB) {
	var hasGroupCol int
	if err := db.QueryRow(`SELECT COUNT(*) FROM pragma_table_info('model_routes') WHERE name = 'group'`).Scan(&hasGroupCol); err != nil || hasGroupCol == 0 {
		return
	}

	rows, err := db.Query(`SELECT DISTINCT "group" FROM model_routes WHERE COALESCE("group", '') <> ''`)
	if err != nil {
		log.Warnf("Legacy group migration skipped: %v", err)
		return
	}
	var groupNames []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err == nil && name != "" {
			groupNames = append(groupNames, name)
		}
	}
	rows.Close()

	for _, name := range groupNames {
		var model string
		if err := db.QueryRow(`SELECT model FROM model_routes WHERE "group" = ? GROUP BY model ORDER BY COUNT(*) DESC LIMIT 1`, name).Scan(&model); err != nil || model == "" {
			continue
		}

		res, err := db.Exec(`INSERT OR IGNORE INTO failover_groups (name, model, cooldown_seconds, enabled, created_at, updated_at)
		                     VALUES (?, ?, 600, 1, ?, ?)`, name, model, time.Now(), time.Now())
		if err != nil {
			log.Warnf("Failed to migrate legacy group %q: %v", name, err)
			continue
		}
		groupID, _ := res.LastInsertId()
		if groupID == 0 {
			_ = db.QueryRow(`SELECT id FROM failover_groups WHERE model = ?`, model).Scan(&groupID)
		}
		if groupID == 0 {
			continue
		}

		var ids []int64
		idRows, err := db.Query(`SELECT id FROM model_routes WHERE "group" = ? ORDER BY id ASC`, name)
		if err != nil {
			continue
		}
		for idRows.Next() {
			var id int64
			if err := idRows.Scan(&id); err == nil {
				ids = append(ids, id)
			}
		}
		idRows.Close()

		for i, id := range ids {
			db.Exec(`UPDATE model_routes SET failover_group_id = ?, priority = ?, model = ? WHERE id = ?`, groupID, i, model, id)
		}
		log.Infof("Migrated legacy group %q -> failover group %d (%d endpoints)", name, groupID, len(ids))
	}

	// 旧的展示字段已无意义，删除
	if _, err := db.Exec(`ALTER TABLE model_routes DROP COLUMN "group"`); err != nil {
		log.Debugf("Drop legacy group column skipped: %v", err)
	}
}
