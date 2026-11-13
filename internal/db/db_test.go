package db

import (
	"context"
	"path/filepath"
	"testing"
)

func TestMigrateIsIdempotent(t *testing.T) {
	ctx := context.Background()
	conn, err := Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()

	for i := 0; i < 2; i++ {
		if err := Migrate(ctx, conn, nil); err != nil {
			t.Fatalf("migrate run %d: %v", i+1, err)
		}
	}
	var n int
	if err := conn.QueryRow(`SELECT COUNT(*) FROM schema_migrations`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n == 0 {
		t.Fatal("expected recorded migrations")
	}

	var fk int
	if err := conn.QueryRow(`PRAGMA foreign_keys`).Scan(&fk); err != nil {
		t.Fatal(err)
	}
	if fk != 1 {
		t.Fatalf("foreign keys not enabled")
	}
}

func TestDuplicateActiveJobRejected(t *testing.T) {
	ctx := context.Background()
	conn, err := Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if err := Migrate(ctx, conn, nil); err != nil {
		t.Fatal(err)
	}
	mustExec := func(q string, args ...any) error {
		_, err := conn.Exec(q, args...)
		return err
	}
	if err := mustExec(`INSERT INTO users(email, display_name, password_hash) VALUES ('a@b.c','A','x')`); err != nil {
		t.Fatal(err)
	}
	if err := mustExec(`INSERT INTO videos(user_id, source_type, title) VALUES (1,'upload','v')`); err != nil {
		t.Fatal(err)
	}
	if err := mustExec(`INSERT INTO processing_jobs(video_id, kind) VALUES (1,'process_video')`); err != nil {
		t.Fatal(err)
	}
	if err := mustExec(`INSERT INTO processing_jobs(video_id, kind) VALUES (1,'process_video')`); err == nil {
		t.Fatal("expected unique violation for second active job")
	}
	// Once completed, a new job may be queued.
	if err := mustExec(`UPDATE processing_jobs SET status='completed'`); err != nil {
		t.Fatal(err)
	}
	if err := mustExec(`INSERT INTO processing_jobs(video_id, kind) VALUES (1,'process_video')`); err != nil {
		t.Fatalf("expected new job after completion: %v", err)
	}

	// Deleting the video cascades.
	if err := mustExec(`DELETE FROM videos WHERE id=1`); err != nil {
		t.Fatal(err)
	}
	var n int
	conn.QueryRow(`SELECT COUNT(*) FROM processing_jobs`).Scan(&n)
	if n != 0 {
		t.Fatalf("expected jobs to cascade, got %d", n)
	}
}
