package storage

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
)

func runPartitionedOnlineIndex(ctx context.Context, db *sql.DB, statement, index, table string) error {
	if !isSafeSQLIdentifier(table) {
		return fmt.Errorf("storage: invalid index table")
	}
	rows, err := db.QueryContext(ctx, `SELECT c.relname,c.oid FROM pg_inherits i JOIN pg_class c ON c.oid=i.inhrelid WHERE i.inhparent=to_regclass($1) ORDER BY c.oid`, table)
	if err != nil {
		return err
	}
	type child struct {
		name string
		oid  uint32
	}
	children := []child{}
	for rows.Next() {
		var item child
		if err = rows.Scan(&item.name, &item.oid); err != nil {
			rows.Close()
			return err
		}
		if !isSafeSQLIdentifier(item.name) {
			rows.Close()
			return fmt.Errorf("storage: invalid partition name")
		}
		children = append(children, item)
	}
	if err = rows.Err(); err != nil {
		rows.Close()
		return err
	}
	rows.Close()
	parent := strings.Replace(statement, "CONCURRENTLY ", "", 1)
	if len(children) > 0 {
		parent = strings.Replace(parent, " ON "+table, " ON ONLY "+table, 1)
	}
	if _, err = db.ExecContext(ctx, parent); err != nil {
		return err
	}
	tail := strings.SplitN(statement, " ON "+table, 2)
	if len(tail) != 2 {
		return fmt.Errorf("storage: invalid partitioned index statement")
	}
	for _, item := range children {
		name := fmt.Sprintf("%s_%d", index, item.oid)
		if len(name) > 63 {
			return fmt.Errorf("storage: partition index name too long")
		}
		var valid bool
		err = db.QueryRowContext(ctx, `SELECT indisvalid FROM pg_index WHERE indexrelid=to_regclass($1)`, name).Scan(&valid)
		if err == nil && !valid {
			if _, err = db.ExecContext(ctx, "DROP INDEX CONCURRENTLY "+name); err != nil {
				return err
			}
		} else if err != nil && err != sql.ErrNoRows {
			return err
		}
		if _, err = db.ExecContext(ctx, "CREATE INDEX CONCURRENTLY IF NOT EXISTS "+name+" ON "+item.name+tail[1]); err != nil {
			return err
		}
		var attached bool
		if err = db.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM pg_inherits WHERE inhparent=to_regclass($1) AND inhrelid=to_regclass($2))`, index, name).Scan(&attached); err != nil {
			return err
		}
		if !attached {
			if _, err = db.ExecContext(ctx, "ALTER INDEX "+index+" ATTACH PARTITION "+name); err != nil {
				return err
			}
		}
	}
	return nil
}
