package spdb

import (
	"context"
	"database/sql"
	"fmt"
	"time"
)

// Snapshot identifies which point in time a database represents.
//
// Every target this tool reaches is a restore taken at some moment, and none
// of them is a full copy of production. That makes "which server" an
// incomplete answer: hrm on the development container and hrm on UAT are the
// same name over different weeks of data, and a finding from one says nothing
// about the other.
//
// Both fields come from the server rather than from configuration. A restore
// date somebody typed into a YAML file is a date that will be wrong eventually;
// sys.databases.create_date cannot be.
type Snapshot struct {
	// Created is when the database was created or restored.
	Created time.Time `json:"created"`
	// LastProcModified is the newest CREATE/ALTER among user procedures, which
	// dates the code in the snapshot rather than the restore that carried it.
	// The two differ: the container's hrm was restored on 2026-02-09 carrying
	// procedures last changed on 2026-03-19.
	LastProcModified time.Time `json:"last_proc_modified,omitempty"`
	// Procedures counts user procedures, a cheap shape check. A snapshot with
	// far fewer than its siblings is a partial restore, not just an older one.
	Procedures int `json:"procedures"`
}

// String renders the snapshot for a report header.
func (s Snapshot) String() string {
	if s.Created.IsZero() {
		return "unknown snapshot"
	}
	out := "restored " + s.Created.Format("2006-01-02")
	if !s.LastProcModified.IsZero() {
		out += ", procedures last changed " + s.LastProcModified.Format("2006-01-02")
	}
	return fmt.Sprintf("%s, %d procedures", out, s.Procedures)
}

// SnapshotOf reads the current database's snapshot markers.
func SnapshotOf(ctx context.Context, db *sql.DB) (Snapshot, error) {
	const q = `
SELECT
    (SELECT create_date FROM sys.databases WHERE name = DB_NAME()),
    (SELECT MAX(modify_date) FROM sys.procedures WHERE is_ms_shipped = 0),
    (SELECT COUNT(*) FROM sys.procedures WHERE is_ms_shipped = 0)`

	var s Snapshot
	var created, modified sql.NullTime
	if err := db.QueryRowContext(ctx, q).Scan(&created, &modified, &s.Procedures); err != nil {
		return Snapshot{}, fmt.Errorf("read snapshot markers: %w", err)
	}
	s.Created = created.Time
	s.LastProcModified = modified.Time
	return s, nil
}
