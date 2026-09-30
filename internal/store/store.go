// Package store opens the SQLite database and applies the schema.
package store

import (
	"database/sql"
	"fmt"

	_ "modernc.org/sqlite"
)

const customersSchema = `
CREATE TABLE IF NOT EXISTS customers (
	id           TEXT PRIMARY KEY,
	wallet       TEXT NOT NULL UNIQUE,
	api_key_hash TEXT NOT NULL UNIQUE,
	available    INTEGER NOT NULL DEFAULT 0 CHECK (available >= 0),
	held         INTEGER NOT NULL DEFAULT 0 CHECK (held >= 0),
	created_at   INTEGER NOT NULL
);
`

// Open opens (creating if needed) the SQLite database at path.
//
// The pool is limited to one connection, so every transaction is serialized.
// That is what keeps Ledger operations race-free on a single backend process.
// Extra schemas (owned by other modules) are applied after the core schema.
func Open(path string, extra ...string) (*sql.DB, error) {
	db, err := sql.Open("sqlite", "file:"+path+"?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)&_pragma=foreign_keys(1)")
	if err != nil {
		return nil, fmt.Errorf("open sqlite: %w", err)
	}
	db.SetMaxOpenConns(1)
	for _, schema := range append([]string{customersSchema}, extra...) {
		if _, err := db.Exec(schema); err != nil {
			db.Close()
			return nil, fmt.Errorf("apply schema: %w", err)
		}
	}
	return db, nil
}
