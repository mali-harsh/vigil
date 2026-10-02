package store

import "database/sql"

// DBForTest exposes the pool for fault-injection tests (ha package).
func DBForTest(s *Store) *sql.DB { return s.db }
