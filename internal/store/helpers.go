package store

import (
	"database/sql"
	"database/sql/driver"
	"fmt"
	"time"
)

// scanner 抽象 sql.Row 与 sql.Rows 共用的 Scan 方法
type scanner interface {
	Scan(dest ...any) error
}

// timeToUnix 将 time.Time 转换为 unix 秒时间戳
func timeToUnix(t time.Time) int64 {
	return t.Unix()
}

// timePtrToUnix 将可空 *time.Time 转换为可写入 SQL 的值，nil 时写 NULL
func timePtrToUnix(t *time.Time) driver.Value {
	if t == nil {
		return nil
	}
	return t.Unix()
}

// nullString 将空字符串转换为 SQL NULL，便于区分“无值”与“空串”
func nullString(s string) driver.Value {
	if s == "" {
		return nil
	}
	return s
}

// unixToTime 将 unix 秒时间戳转换为 time.Time
func unixToTime(sec int64) time.Time {
	return time.Unix(sec, 0)
}

// nullInt64ToTimePtr 将可空时间戳列转换为 *time.Time
func nullInt64ToTimePtr(v sql.NullInt64) *time.Time {
	if !v.Valid {
		return nil
	}
	t := unixToTime(v.Int64)
	return &t
}

// checkRowsAffected 在更新影响行数为 0 时返回“不存在”错误
func checkRowsAffected(res sql.Result, label, id string) error {
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("获取受影响行数失败: %w", err)
	}
	if n == 0 {
		return fmt.Errorf("%s不存在: %s", label, id)
	}
	return nil
}
