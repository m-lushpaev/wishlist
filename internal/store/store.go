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

	_ "modernc.org/sqlite"
)

var ErrReserved = errors.New("wish is already reserved")

type Reservation struct {
	WishID    string
	Name      string
	Note      string
	CreatedAt time.Time
}

type DB struct {
	db *sql.DB
}

func Open(path string) (*DB, error) {
	database, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, err
	}
	database.SetMaxOpenConns(1)
	for _, statement := range []string{
		"PRAGMA journal_mode=WAL",
		"PRAGMA busy_timeout=5000",
		"PRAGMA foreign_keys=ON",
		`CREATE TABLE IF NOT EXISTS reservations (
            wish_id TEXT PRIMARY KEY,
            reserver_name TEXT NOT NULL,
            note TEXT NOT NULL,
            cancel_hash BLOB NOT NULL UNIQUE,
            created_at TEXT NOT NULL
        )`,
	} {
		if _, err := database.Exec(statement); err != nil {
			database.Close()
			return nil, fmt.Errorf("initialize database: %w", err)
		}
	}
	return &DB{db: database}, nil
}

func (database *DB) Close() error { return database.db.Close() }

func (database *DB) Ping(ctx context.Context) error { return database.db.PingContext(ctx) }

func (database *DB) List(ctx context.Context) (map[string]Reservation, error) {
	rows, err := database.db.QueryContext(ctx, `SELECT wish_id, reserver_name, note, created_at FROM reservations ORDER BY created_at`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := map[string]Reservation{}
	for rows.Next() {
		var reservation Reservation
		var timestamp string
		if err := rows.Scan(&reservation.WishID, &reservation.Name, &reservation.Note, &timestamp); err != nil {
			return nil, err
		}
		reservation.CreatedAt, err = time.Parse(time.RFC3339Nano, timestamp)
		if err != nil {
			return nil, fmt.Errorf("invalid reservation timestamp: %w", err)
		}
		result[reservation.WishID] = reservation
	}
	return result, rows.Err()
}

func (database *DB) Reserve(ctx context.Context, wishID, name, note string) (string, error) {
	tokenBytes := make([]byte, 32)
	if _, err := rand.Read(tokenBytes); err != nil {
		return "", err
	}
	token := base64.RawURLEncoding.EncodeToString(tokenBytes)
	hash := sha256.Sum256([]byte(token))
	_, err := database.db.ExecContext(ctx,
		`INSERT INTO reservations(wish_id, reserver_name, note, cancel_hash, created_at) VALUES(?, ?, ?, ?, ?)`,
		wishID, strings.TrimSpace(name), strings.TrimSpace(note), hash[:], time.Now().UTC().Format(time.RFC3339Nano))
	if err != nil {
		if strings.Contains(strings.ToLower(err.Error()), "unique constraint") {
			return "", ErrReserved
		}
		return "", err
	}
	return token, nil
}

func (database *DB) FindByToken(ctx context.Context, token string) (Reservation, error) {
	hash := sha256.Sum256([]byte(token))
	var reservation Reservation
	var timestamp string
	err := database.db.QueryRowContext(ctx,
		`SELECT wish_id, reserver_name, note, created_at FROM reservations WHERE cancel_hash = ?`, hash[:]).
		Scan(&reservation.WishID, &reservation.Name, &reservation.Note, &timestamp)
	if err != nil {
		return Reservation{}, err
	}
	reservation.CreatedAt, err = time.Parse(time.RFC3339Nano, timestamp)
	return reservation, err
}

func (database *DB) Cancel(ctx context.Context, token string) (bool, error) {
	hash := sha256.Sum256([]byte(token))
	result, err := database.db.ExecContext(ctx, `DELETE FROM reservations WHERE cancel_hash = ?`, hash[:])
	if err != nil {
		return false, err
	}
	count, err := result.RowsAffected()
	return count == 1, err
}
