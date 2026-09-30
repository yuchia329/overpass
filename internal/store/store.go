// Package store opens the SQLite database and applies module schemas.
package store

import (
	"database/sql"
	"fmt"

	_ "modernc.org/sqlite"
)

// Open opens (creating if needed) the SQLite database at path.
//
// The pool is limited to one connection, so every transaction is serialized.
// That is what keeps Ledger operations race-free on a single backend process.
// Schemas are owned by their modules and applied in the order given.
func Open(path string, schemas ...string) (*sql.DB, error) {
	db, err := sql.Open("sqlite", "file:"+path+"?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)&_pragma=foreign_keys(1)")
	if err != nil {
		return nil, fmt.Errorf("open sqlite: %w", err)
	}
	db.SetMaxOpenConns(1)
	for _, schema := range schemas {
		if _, err := db.Exec(schema); err != nil {
			db.Close()
			return nil, fmt.Errorf("apply schema: %w", err)
		}
	}
	return db, nil
}
