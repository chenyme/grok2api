package relational

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"time"

	glebarezsqlite "github.com/glebarez/sqlite"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

// Database 持有关系型数据库连接和各仓储实现共享的 GORM 实例。
type Database struct {
	db      *gorm.DB
	dialect string
}

func (d *Database) Stats() sql.DBStats {
	if d == nil {
		return sql.DBStats{}
	}
	sqlDB, err := d.db.DB()
	if err != nil {
		return sql.DBStats{}
	}
	return sqlDB.Stats()
}

func (d *Database) Dialect() string {
	if d == nil {
		return ""
	}
	return d.dialect
}

// OpenSQLite 打开纯 Go SQLite 数据库并启用 WAL、外键与 busy timeout。
// 显式事务使用 IMMEDIATE，避免并发读后写事务在锁升级时直接返回 SQLITE_BUSY。
func OpenSQLite(ctx context.Context, path string) (*Database, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, fmt.Errorf("创建数据库目录: %w", err)
	}
	dsn := fmt.Sprintf("file:%s?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)&_pragma=foreign_keys(1)&_txlock=immediate", path)
	db, err := gorm.Open(glebarezsqlite.Open(dsn), gormConfig())
	if err != nil {
		return nil, fmt.Errorf("打开 SQLite: %w", err)
	}
	return configureDatabase(ctx, db, "sqlite", 16, 16)
}

// OpenPostgres 打开 PostgreSQL 数据库并配置连接池。
func OpenPostgres(ctx context.Context, dsn string, maxOpenConns, maxIdleConns int) (*Database, error) {
	db, err := gorm.Open(postgres.Open(dsn), gormConfig())
	if err != nil {
		return nil, fmt.Errorf("打开 PostgreSQL: %w", err)
	}
	return configureDatabase(ctx, db, "postgres", maxOpenConns, maxIdleConns)
}

func gormConfig() *gorm.Config {
	return &gorm.Config{
		Logger:         logger.Default.LogMode(logger.Silent),
		TranslateError: true,
		NowFunc:        func() time.Time { return time.Now().UTC() },
	}
}

func configureDatabase(ctx context.Context, db *gorm.DB, dialect string, maxOpenConns, maxIdleConns int) (*Database, error) {
	sqlDB, err := db.DB()
	if err != nil {
		return nil, err
	}
	sqlDB.SetMaxOpenConns(maxOpenConns)
	sqlDB.SetMaxIdleConns(maxIdleConns)
	sqlDB.SetConnMaxLifetime(time.Hour)
	if err := sqlDB.PingContext(ctx); err != nil {
		_ = sqlDB.Close()
		return nil, fmt.Errorf("连接 %s: %w", dialect, err)
	}
	return &Database{db: db, dialect: dialect}, nil
}

// Close 关闭底层数据库连接。
func (d *Database) Close() error {
	sqlDB, err := d.db.DB()
	if err != nil {
		return err
	}
	return sqlDB.Close()
}
