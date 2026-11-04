package store

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/m-lushpaev/wishlist/internal/profile"
	_ "modernc.org/sqlite"
)

var ErrReserved = errors.New("wish is already reserved")
var ErrConflict = errors.New("catalog has changed")
var ErrMigrationRequired = errors.New("legacy database: run migrate-db before starting the server")

type Reservation struct {
	Profile, WishID, WishName, Name, Note string
	CreatedAt                             time.Time
}

type DB struct{ db *sql.DB }

const schema = `CREATE TABLE reservations (
 profile TEXT NOT NULL CHECK(profile IN ('maksim','anya','grisha','arisha')),
 wish_id TEXT NOT NULL, wish_name TEXT NOT NULL DEFAULT '',
 reserver_name TEXT NOT NULL, note TEXT NOT NULL,
 cancel_hash BLOB NOT NULL UNIQUE, created_at TEXT NOT NULL,
 PRIMARY KEY(profile,wish_id));
 CREATE TABLE catalog_overrides (
 profile TEXT NOT NULL CHECK(profile IN ('maksim','anya','grisha','arisha')),
 wish_id TEXT NOT NULL, name TEXT NOT NULL, url TEXT NOT NULL,
 description TEXT NOT NULL, note TEXT NOT NULL, image TEXT NOT NULL, category TEXT NOT NULL,
 hidden INTEGER NOT NULL CHECK(hidden IN (0,1)), revision INTEGER NOT NULL CHECK(revision>0), updated_at TEXT NOT NULL,
 PRIMARY KEY(profile,wish_id));
 CREATE TABLE media (
 profile TEXT NOT NULL CHECK(profile IN ('maksim','anya','grisha','arisha')),
 digest TEXT NOT NULL, mime TEXT NOT NULL, body BLOB NOT NULL,
 PRIMARY KEY(profile,digest));
 CREATE TABLE catalog_clock(id INTEGER PRIMARY KEY CHECK(id=1), version INTEGER NOT NULL);
 INSERT INTO catalog_clock(id,version) VALUES(1,0);`

func connect(path string) (*sql.DB, error) {
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	for _, s := range []string{"PRAGMA busy_timeout=5000", "PRAGMA foreign_keys=ON"} {
		if _, err = db.Exec(s); err != nil {
			db.Close()
			return nil, err
		}
	}
	return db, nil
}

func Open(path string) (*DB, error)    { return open(path, false) }
func Migrate(path string) (*DB, error) { return open(path, true) }

func open(path string, migrate bool) (*DB, error) {
	db, err := connect(path)
	if err != nil {
		return nil, err
	}
	if err = initialize(db, migrate); err != nil {
		db.Close()
		return nil, err
	}
	if _, err = db.Exec("PRAGMA journal_mode=WAL"); err != nil {
		db.Close()
		return nil, err
	}
	return &DB{db: db}, nil
}

func initialize(db *sql.DB, migrate bool) error {
	var version, tables int
	if err := db.QueryRow("PRAGMA user_version").Scan(&version); err != nil {
		return err
	}
	if version > 1 || version < 0 {
		return fmt.Errorf("unsupported schema version %d", version)
	}
	if version == 1 {
		return verifySchema(db)
	}
	if err := db.QueryRow("SELECT count(*) FROM sqlite_master WHERE type='table' AND name NOT LIKE 'sqlite_%'").Scan(&tables); err != nil {
		return err
	}
	if tables > 0 {
		if tables != 1 {
			return errors.New("unexpected legacy database schema")
		}
		if err := checkColumns(db, "reservations", []string{"wish_id", "reserver_name", "note", "cancel_hash", "created_at"}); err != nil {
			return err
		}
		var ddl string
		if err := db.QueryRow("SELECT sql FROM sqlite_master WHERE type='table' AND name='reservations'").Scan(&ddl); err != nil {
			return err
		}
		normalize := func(s string) string {
			return strings.ReplaceAll(strings.Join(strings.Fields(strings.ToLower(s)), ""), "ifnotexists", "")
		}
		expected := `CREATE TABLE reservations(wish_id TEXT PRIMARY KEY,reserver_name TEXT NOT NULL,note TEXT NOT NULL,cancel_hash BLOB NOT NULL UNIQUE,created_at TEXT NOT NULL)`
		if normalize(ddl) != normalize(expected) {
			return errors.New("unexpected legacy database constraints")
		}
		if !migrate {
			return ErrMigrationRequired
		}
	}
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if tables > 0 {
		if _, err = tx.Exec("ALTER TABLE reservations RENAME TO legacy_reservations"); err != nil {
			return err
		}
	}
	if _, err = tx.Exec(schema); err != nil {
		return err
	}
	if tables > 0 {
		if _, err = tx.Exec(`INSERT INTO reservations(profile,wish_id,wish_name,reserver_name,note,cancel_hash,created_at)
    SELECT 'maksim',wish_id,'',reserver_name,note,cancel_hash,created_at FROM legacy_reservations`); err != nil {
			return err
		}
		var missing, oldCount, newCount int
		if err = tx.QueryRow(`SELECT count(*) FROM legacy_reservations o LEFT JOIN reservations n ON n.profile='maksim' AND n.wish_id=o.wish_id
    WHERE n.wish_id IS NULL OR n.reserver_name IS NOT o.reserver_name OR n.note IS NOT o.note OR n.cancel_hash IS NOT o.cancel_hash OR n.created_at IS NOT o.created_at`).Scan(&missing); err != nil {
			return err
		}
		if err = tx.QueryRow("SELECT count(*) FROM legacy_reservations").Scan(&oldCount); err != nil {
			return err
		}
		if err = tx.QueryRow("SELECT count(*) FROM reservations").Scan(&newCount); err != nil {
			return err
		}
		if missing != 0 || oldCount != newCount {
			return errors.New("migration comparison failed")
		}
		if _, err = tx.Exec("DROP TABLE legacy_reservations"); err != nil {
			return err
		}
	}
	if _, err = tx.Exec("PRAGMA user_version=1"); err != nil {
		return err
	}
	var integrity string
	if err = tx.QueryRow("PRAGMA integrity_check").Scan(&integrity); err != nil {
		return err
	}
	if integrity != "ok" {
		return errors.New("database integrity check failed")
	}
	return tx.Commit()
}

func checkColumns(db *sql.DB, table string, expected []string) error {
	rows, err := db.Query("PRAGMA table_info(" + table + ")")
	if err != nil {
		return err
	}
	defer rows.Close()
	i := 0
	for rows.Next() {
		var cid, notnull, pk int
		var name, typ string
		var def any
		if err = rows.Scan(&cid, &name, &typ, &notnull, &def, &pk); err != nil {
			return err
		}
		if i >= len(expected) || name != expected[i] {
			return errors.New("unexpected database columns")
		}
		i++
	}
	if err = rows.Err(); err != nil {
		return err
	}
	if i != len(expected) {
		return errors.New("unexpected database columns")
	}
	return nil
}

func verifySchema(db *sql.DB) error {
	for _, v := range []struct {
		table string
		cols  []string
	}{
		{"reservations", []string{"profile", "wish_id", "wish_name", "reserver_name", "note", "cancel_hash", "created_at"}},
		{"catalog_overrides", []string{"profile", "wish_id", "name", "url", "description", "note", "image", "category", "hidden", "revision", "updated_at"}},
		{"media", []string{"profile", "digest", "mime", "body"}},
		{"catalog_clock", []string{"id", "version"}},
	} {
		if err := checkColumns(db, v.table, v.cols); err != nil {
			return err
		}
	}
	return nil
}

func (d *DB) Close() error                   { return d.db.Close() }
func (d *DB) Ping(ctx context.Context) error { return d.db.PingContext(ctx) }
func (d *DB) Count(ctx context.Context) (int, error) {
	var count int
	err := d.db.QueryRowContext(ctx, "SELECT count(*) FROM reservations").Scan(&count)
	return count, err
}
func validProfile(p string) error {
	if _, ok := profile.Lookup(p); !ok {
		return errors.New("unknown profile")
	}
	return nil
}

func (d *DB) List(ctx context.Context, p string) (map[string]Reservation, error) {
	if err := validProfile(p); err != nil {
		return nil, err
	}
	rows, err := d.db.QueryContext(ctx, `SELECT profile,wish_id,wish_name,reserver_name,note,created_at FROM reservations WHERE profile=? ORDER BY created_at`, p)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := map[string]Reservation{}
	for rows.Next() {
		var r Reservation
		var timestamp string
		if err = rows.Scan(&r.Profile, &r.WishID, &r.WishName, &r.Name, &r.Note, &timestamp); err != nil {
			return nil, err
		}
		r.CreatedAt, err = time.Parse(time.RFC3339Nano, timestamp)
		if err != nil {
			return nil, err
		}
		result[r.WishID] = r
	}
	return result, rows.Err()
}

func (d *DB) Reserve(ctx context.Context, p, id, title, name, note string) (string, error) {
	if err := validProfile(p); err != nil {
		return "", err
	}
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	token := base64.RawURLEncoding.EncodeToString(b)
	hash := sha256.Sum256([]byte(token))
	_, err := d.db.ExecContext(ctx, `INSERT INTO reservations(profile,wish_id,wish_name,reserver_name,note,cancel_hash,created_at) VALUES(?,?,?,?,?,?,?)`, p, id, title, strings.TrimSpace(name), strings.TrimSpace(note), hash[:], time.Now().UTC().Format(time.RFC3339Nano))
	if err != nil {
		if strings.Contains(strings.ToLower(err.Error()), "unique constraint") {
			return "", ErrReserved
		}
		return "", err
	}
	return token, nil
}

func (d *DB) FindByToken(ctx context.Context, token string) (Reservation, error) {
	hash := sha256.Sum256([]byte(token))
	var r Reservation
	var timestamp string
	err := d.db.QueryRowContext(ctx, `SELECT profile,wish_id,wish_name,reserver_name,note,created_at FROM reservations WHERE cancel_hash=?`, hash[:]).Scan(&r.Profile, &r.WishID, &r.WishName, &r.Name, &r.Note, &timestamp)
	if err != nil {
		return Reservation{}, err
	}
	r.CreatedAt, err = time.Parse(time.RFC3339Nano, timestamp)
	return r, err
}

func (d *DB) Cancel(ctx context.Context, token string) (bool, error) {
	hash := sha256.Sum256([]byte(token))
	result, err := d.db.ExecContext(ctx, `DELETE FROM reservations WHERE cancel_hash=?`, hash[:])
	if err != nil {
		return false, err
	}
	n, err := result.RowsAffected()
	return n == 1, err
}
