package mobilepush

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"time"

	"gorm.io/gorm"
)

// A session lock spans independently committed phases on one checked-out
// connection. Waiters never reserve the extra connections needed by a holder.
func withDatabaseLock(ctx context.Context, db *gorm.DB, key int64, operation func(*gorm.DB) error) error {
	return db.WithContext(ctx).Connection(func(conn *gorm.DB) (err error) {
		physical, ok := conn.Statement.ConnPool.(*sql.Conn)
		if !ok {
			return errors.New("push lock requires a dedicated database connection")
		}
		locked := false
		defer func() {
			if locked {
				cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
				defer cancel()
				var released bool
				unlockErr := conn.WithContext(cleanupCtx).Raw("SELECT pg_advisory_unlock(?)", key).Scan(&released).Error
				if unlockErr == nil && released {
					return
				}
				err = errors.Join(err, errors.New("release push database lock failed"))
			}
			// A canceled acquisition can have acquired the session lock just
			// before its response was lost. Never return that connection to the pool.
			_ = physical.Raw(func(any) error { return driver.ErrBadConn })
		}()
		if err := conn.Exec("SELECT pg_advisory_lock(?)", key).Error; err != nil {
			return err
		}
		locked = true
		return operation(conn.Session(&gorm.Session{NewDB: true}))
	})
}
