package store

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"errors"
	"path/filepath"
	"sync"
	"testing"
)

func TestReservationLifecycle(t *testing.T) {
	database, err := Open(filepath.Join(t.TempDir(), "wishlist.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	ctx := context.Background()
	token, err := database.Reserve(ctx, "maksim", "bike", "Bike", "Friend", "Size M")
	if err != nil || token == "" {
		t.Fatalf("reserve: token=%q err=%v", token, err)
	}
	if _, err := database.Reserve(ctx, "maksim", "bike", "Bike", "Other", ""); !errors.Is(err, ErrReserved) {
		t.Fatalf("duplicate reserve: %v", err)
	}
	reservation, err := database.FindByToken(ctx, token)
	if err != nil || reservation.WishID != "bike" || reservation.Name != "Friend" {
		t.Fatalf("find: %#v err=%v", reservation, err)
	}
	if removed, err := database.Cancel(ctx, token); err != nil || !removed {
		t.Fatalf("cancel: removed=%v err=%v", removed, err)
	}
	if removed, err := database.Cancel(ctx, token); err != nil || removed {
		t.Fatalf("second cancel: removed=%v err=%v", removed, err)
	}
}

func TestLegacyMigrationPreservesTokenAndValues(t *testing.T) {
	path := filepath.Join(t.TempDir(), "legacy.db")
	old, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	_, err = old.Exec(`CREATE TABLE reservations(wish_id TEXT PRIMARY KEY,reserver_name TEXT NOT NULL,note TEXT NOT NULL,cancel_hash BLOB NOT NULL UNIQUE,created_at TEXT NOT NULL)`)
	if err != nil {
		t.Fatal(err)
	}
	token := "old-secret-token"
	hash := sha256.Sum256([]byte(token))
	_, err = old.Exec("INSERT INTO reservations VALUES(?,?,?,?,?)", "bike", "Friend", "Size M", hash[:], "2026-10-01T12:13:14.123456789Z")
	if err != nil {
		t.Fatal(err)
	}
	old.Close()
	if d, e := Open(path); !errors.Is(e, ErrMigrationRequired) {
		if d != nil {
			d.Close()
		}
		t.Fatalf("implicit migration: %v", e)
	}
	d, err := Migrate(path)
	if err != nil {
		t.Fatal(err)
	}
	var gotHash []byte
	if err = d.db.QueryRow("SELECT cancel_hash FROM reservations").Scan(&gotHash); err != nil || !bytes.Equal(gotHash, hash[:]) {
		t.Fatal("hash changed", err)
	}
	r, err := d.FindByToken(context.Background(), token)
	if err != nil || r.Profile != "maksim" || r.WishID != "bike" || r.Name != "Friend" || r.Note != "Size M" || r.CreatedAt.Nanosecond() != 123456789 {
		t.Fatalf("migration: %#v %v", r, err)
	}
	d.Close()
	d, err = Migrate(path)
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	if n, e := d.Count(context.Background()); e != nil || n != 1 {
		t.Fatalf("repeat count=%d err=%v", n, e)
	}
	if ok, e := d.Cancel(context.Background(), token); e != nil || !ok {
		t.Fatal("old token failed", e)
	}
}

func TestProfilesAndConcurrentReservation(t *testing.T) {
	d, err := Open(filepath.Join(t.TempDir(), "app.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	ctx := context.Background()
	if _, err = d.Reserve(ctx, "anya", "bike", "Bike", "A", ""); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	var mu sync.Mutex
	successes := 0
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, e := d.Reserve(ctx, "maksim", "bike", "Bike", "M", "")
			mu.Lock()
			defer mu.Unlock()
			if e == nil {
				successes++
			} else if !errors.Is(e, ErrReserved) {
				t.Error(e)
			}
		}()
	}
	wg.Wait()
	if successes != 1 {
		t.Fatalf("successes=%d", successes)
	}
	for _, p := range []string{"anya", "maksim"} {
		rows, e := d.List(ctx, p)
		if e != nil || len(rows) != 1 {
			t.Fatalf("%s: %#v %v", p, rows, e)
		}
	}
}

func TestMigrationRejectsUnexpectedSchemaWithoutChanges(t *testing.T) {
	path := filepath.Join(t.TempDir(), "bad.db")
	old, _ := sql.Open("sqlite", path)
	old.Exec("CREATE TABLE reservations(wish_id TEXT PRIMARY KEY, extra TEXT)")
	old.Exec("INSERT INTO reservations VALUES('keep','value')")
	old.Close()
	if d, e := Migrate(path); e == nil {
		d.Close()
		t.Fatal("unexpected schema migrated")
	}
	old, _ = sql.Open("sqlite", path)
	defer old.Close()
	var value string
	if e := old.QueryRow("SELECT extra FROM reservations WHERE wish_id='keep'").Scan(&value); e != nil || value != "value" {
		t.Fatal("source changed", e)
	}
}

func TestFailedMigrationRollsBackAllSchemaChanges(t *testing.T) {
	path := filepath.Join(t.TempDir(), "legacy.db")
	old, _ := sql.Open("sqlite", path)
	old.Exec(`CREATE TABLE reservations(wish_id TEXT PRIMARY KEY,reserver_name TEXT NOT NULL,note TEXT NOT NULL,cancel_hash BLOB NOT NULL UNIQUE,created_at TEXT NOT NULL)`)
	old.Exec(`INSERT INTO reservations VALUES(NULL,'keep','note',X'01','2026-10-01T00:00:00Z')`)
	old.Close()
	if d, e := Migrate(path); e == nil {
		d.Close()
		t.Fatal("invalid row migrated")
	}
	old, _ = sql.Open("sqlite", path)
	defer old.Close()
	var name string
	var version, tables int
	if e := old.QueryRow("SELECT reserver_name FROM reservations").Scan(&name); e != nil || name != "keep" {
		t.Fatal("source row changed", e)
	}
	old.QueryRow("PRAGMA user_version").Scan(&version)
	old.QueryRow("SELECT count(*) FROM sqlite_master WHERE type='table'").Scan(&tables)
	if version != 0 || tables != 1 {
		t.Fatalf("partial migration: version=%d tables=%d", version, tables)
	}
}
