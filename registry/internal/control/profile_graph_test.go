package control

import (
	"database/sql"
	"path/filepath"
	"testing"
)

// TestProfileVersionsLegacyRowBackfill proves the stage-08a ALTER path backfills
// existing text rows: a pre-stage DB has profile_versions without the four new
// columns; after Open the row must read back as format="text" with steps intact.
func TestProfileVersionsLegacyRowBackfill(t *testing.T) {
	path := filepath.Join(t.TempDir(), "legacy.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	for _, ddl := range []string{
		`CREATE TABLE profiles (id INTEGER PRIMARY KEY, name TEXT UNIQUE NOT NULL, description TEXT NOT NULL DEFAULT '', published INTEGER NOT NULL DEFAULT 0, created_by INTEGER NOT NULL DEFAULT 0, created_at INTEGER NOT NULL, updated_at INTEGER NOT NULL)`,
		`CREATE TABLE profile_versions (id INTEGER PRIMARY KEY, profile_id INTEGER NOT NULL REFERENCES profiles(id), version INTEGER NOT NULL, playbook TEXT NOT NULL, vars_schema TEXT NOT NULL DEFAULT '', created_at INTEGER NOT NULL, UNIQUE(profile_id, version))`,
	} {
		if _, err := db.Exec(ddl); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := db.Exec(`INSERT INTO profiles (id,name,description,published,created_by,created_at,updated_at) VALUES (1,'legacy','',0,0,1,1)`); err != nil {
		t.Fatal(err)
	}
	const legacySteps = "# legacy step\necho hi\n"
	if _, err := db.Exec(`INSERT INTO profile_versions (profile_id,version,playbook,vars_schema,created_at) VALUES (1,1,?,'',1)`, legacySteps); err != nil {
		t.Fatal(err)
	}
	_ = db.Close()

	s, err := Open(path, 100)
	if err != nil {
		t.Fatalf("open legacy db: %v", err)
	}
	defer s.Close()
	v, err := s.ProfileVersion(1, 1)
	if err != nil {
		t.Fatalf("read legacy version: %v", err)
	}
	if v.Format != FormatText || v.Steps != legacySteps {
		t.Fatalf("legacy backfill = %+v", v)
	}
}

func TestProfileGraphVersionMigrationIdempotentAndStorage(t *testing.T) {
	path := filepath.Join(t.TempDir(), "control.db")
	s, err := Open(path, 100)
	if err != nil {
		t.Fatal(err)
	}
	p, err := s.CreateProfile("graphy", "d", 0)
	if err != nil {
		t.Fatal(err)
	}
	// text version keeps legacy semantics
	tv, err := s.AddProfileVersion(p.ID, "# step\necho hi\n", "")
	if err != nil {
		t.Fatal(err)
	}
	if tv.Format != FormatText || tv.Version != 1 {
		t.Fatalf("text version = %+v", tv)
	}
	// graph version is immutable, max+1, and resets published
	gv, err := s.CreateProfileVersionGraph(p.ID, `{"schema_version":1}`, 1, "deadbeef")
	if err != nil {
		t.Fatal(err)
	}
	if gv.Format != FormatGraph || gv.Version != 2 || gv.ContentHash != "deadbeef" || gv.Blueprint == "" {
		t.Fatalf("graph version = %+v", gv)
	}
	if _, err := s.CreateProfileVersionGraph(p.ID, "", 1, "x"); err == nil {
		t.Fatalf("empty blueprint must be rejected")
	}
	// numeric lookup, not latest
	got, err := s.ProfileVersionByID(p.ID, 1)
	if err != nil || got.Format != FormatText || got.Steps != tv.Steps {
		t.Fatalf("version 1 lookup = %+v err=%v", got, err)
	}
	got2, err := s.AuthoredVersionFor(p.ID, 2)
	if err != nil || got2.Format != FormatGraph || got2.Blueprint != gv.Blueprint {
		t.Fatalf("version 2 lookup = %+v err=%v", got2, err)
	}
	if _, err := s.ProfileVersionByID(p.ID, 0); err != ErrNotFound {
		t.Fatalf("version 0 must be ErrNotFound, got %v", err)
	}
	// published reset by graph version
	pp, err := s.Profile(p.ID)
	if err != nil || pp.Published {
		t.Fatalf("published must reset, got %+v", pp)
	}
	s.Close()

	// Re-open must be idempotent and preserve rows.
	s2, err := Open(path, 100)
	if err != nil {
		t.Fatal(err)
	}
	defer s2.Close()
	again, err := s2.AuthoredVersionFor(p.ID, 2)
	if err != nil || again.ContentHash != "deadbeef" {
		t.Fatalf("after reopen = %+v err=%v", again, err)
	}
	// third open too (idempotent migrations)
	s3, err := Open(path, 100)
	if err != nil {
		t.Fatal(err)
	}
	s3.Close()
}
