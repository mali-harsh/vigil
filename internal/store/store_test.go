package store

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"
)

func TestMigratesPhase0Database(t *testing.T) {
	path := filepath.Join(t.TempDir(), "old.db")
	db, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(migrations[0]); err != nil { // a phase 0 DB: tables, user_version 0
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO incidents(monitor_id, started_at, cause, last_notified_at) VALUES('web', 1, 'boom', 1)`); err != nil {
		t.Fatal(err)
	}
	db.Close()

	s, err := Open(path, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	inc, err := s.ActiveIncident(context.Background(), "web")
	if err != nil || inc == nil || inc.Status != Investigating || inc.Impact != "down" {
		t.Fatalf("old incident not carried over: %+v %v", inc, err)
	}
	s.Close()
	if s2, err := Open(path, nil); err != nil { // reopening is a no-op
		t.Fatal(err)
	} else {
		s2.Close()
	}
}
