// Package store persists notes and API tokens in SQLite.
package store

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	_ "modernc.org/sqlite"

	"github.com/dvher/pogo_pad/internal/lww"
	"github.com/dvher/pogo_pad/internal/model"
)

// ErrNotFound is returned when a token lookup or revoke matches nothing.
var ErrNotFound = errors.New("not found")

type Store struct {
	db *sql.DB
}

const schema = `
CREATE TABLE IF NOT EXISTS notes (
	id         TEXT PRIMARY KEY,
	content    TEXT    NOT NULL DEFAULT '',
	color      TEXT    NOT NULL DEFAULT '',
	deleted    INTEGER NOT NULL DEFAULT 0,
	updated_at INTEGER NOT NULL,
	device_id  TEXT    NOT NULL DEFAULT '',
	rev        INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS notes_rev ON notes(rev);
CREATE TABLE IF NOT EXISTS meta (
	key   TEXT PRIMARY KEY,
	value TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS tokens (
	id           INTEGER PRIMARY KEY AUTOINCREMENT,
	name         TEXT    NOT NULL,
	hash         TEXT    NOT NULL UNIQUE,
	created_at   INTEGER NOT NULL,
	last_used_at INTEGER,
	revoked      INTEGER NOT NULL DEFAULT 0
);
`

// Open opens (creating if needed) the database at path.
func Open(path string) (*Store, error) {
	if dir := filepath.Dir(path); dir != "" {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return nil, err
		}
	}
	db, err := sql.Open("sqlite", path+"?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)&_pragma=foreign_keys(1)")
	if err != nil {
		return nil, err
	}
	// SQLite allows a single writer; serialising through one connection keeps
	// rev allocation race-free.
	db.SetMaxOpenConns(1)
	if _, err := db.Exec(schema); err != nil {
		db.Close()
		return nil, fmt.Errorf("migrate: %w", err)
	}
	return &Store{db: db}, nil
}

func (s *Store) Close() error { return s.db.Close() }

// Sync applies incoming changes with last-write-wins and returns up to limit
// notes whose rev is greater than cursor.
func (s *Store) Sync(ctx context.Context, cursor int64, changes []model.Note, limit int) (model.SyncResponse, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return model.SyncResponse{}, err
	}
	defer tx.Rollback()

	var maxRev int64
	if err := tx.QueryRowContext(ctx, `SELECT COALESCE(MAX(rev), 0) FROM notes`).Scan(&maxRev); err != nil {
		return model.SyncResponse{}, err
	}

	for _, in := range changes {
		var cur model.Note
		err := tx.QueryRowContext(ctx,
			`SELECT updated_at, device_id FROM notes WHERE id = ?`, in.ID,
		).Scan(&cur.UpdatedAt, &cur.DeviceID)
		switch {
		case errors.Is(err, sql.ErrNoRows):
		case err != nil:
			return model.SyncResponse{}, err
		default:
			if !lww.Wins(in, cur) {
				continue
			}
		}
		maxRev++
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO notes (id, content, color, deleted, updated_at, device_id, rev)
			VALUES (?, ?, ?, ?, ?, ?, ?)
			ON CONFLICT(id) DO UPDATE SET
				content = excluded.content, color = excluded.color, deleted = excluded.deleted,
				updated_at = excluded.updated_at, device_id = excluded.device_id, rev = excluded.rev`,
			in.ID, in.Content, in.Color, in.Deleted, in.UpdatedAt, in.DeviceID, maxRev,
		); err != nil {
			return model.SyncResponse{}, err
		}
	}

	rows, err := tx.QueryContext(ctx, `
		SELECT id, content, color, deleted, updated_at, device_id, rev
		FROM notes WHERE rev > ? ORDER BY rev LIMIT ?`, cursor, limit+1)
	if err != nil {
		return model.SyncResponse{}, err
	}
	resp := model.SyncResponse{Cursor: cursor, Changes: []model.Note{}}
	for rows.Next() {
		var n model.Note
		if err := rows.Scan(&n.ID, &n.Content, &n.Color, &n.Deleted, &n.UpdatedAt, &n.DeviceID, &n.Rev); err != nil {
			rows.Close()
			return model.SyncResponse{}, err
		}
		if len(resp.Changes) == limit {
			resp.More = true
			break
		}
		resp.Changes = append(resp.Changes, n)
		resp.Cursor = n.Rev
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return model.SyncResponse{}, err
	}
	return resp, tx.Commit()
}

// ErrExists is returned by SetMeta when the key is already set and
// overwrite was not requested.
var ErrExists = errors.New("already set")

// GetMeta returns the opaque value stored under key.
func (s *Store) GetMeta(ctx context.Context, key string) (string, error) {
	var v string
	err := s.db.QueryRowContext(ctx, `SELECT value FROM meta WHERE key = ?`, key).Scan(&v)
	if errors.Is(err, sql.ErrNoRows) {
		return "", ErrNotFound
	}
	return v, err
}

// SetMeta stores value under key. Unless overwrite is true it fails with
// ErrExists when the key already has a value.
func (s *Store) SetMeta(ctx context.Context, key, value string, overwrite bool) error {
	q := `INSERT INTO meta (key, value) VALUES (?, ?) ON CONFLICT(key) DO NOTHING`
	if overwrite {
		q = `INSERT INTO meta (key, value) VALUES (?, ?) ON CONFLICT(key) DO UPDATE SET value = excluded.value`
	}
	res, err := s.db.ExecContext(ctx, q, key, value)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrExists
	}
	return nil
}

// DeleteMeta removes key.
func (s *Store) DeleteMeta(ctx context.Context, key string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM meta WHERE key = ?`, key)
	return err
}

// Token is an API token record (the secret itself is never stored).
type Token struct {
	ID         int64
	Name       string
	CreatedAt  time.Time
	LastUsedAt *time.Time
	Revoked    bool
}

func hashToken(secret string) string {
	sum := sha256.Sum256([]byte(secret))
	return hex.EncodeToString(sum[:])
}

// CreateToken generates a new token for name and returns the plaintext secret.
func (s *Store) CreateToken(ctx context.Context, name string) (string, error) {
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	secret := "pogo_" + base64.RawURLEncoding.EncodeToString(buf)
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO tokens (name, hash, created_at) VALUES (?, ?, ?)`,
		name, hashToken(secret), time.Now().Unix())
	return secret, err
}

// Authenticate reports whether secret is a valid, non-revoked token and
// records its use.
func (s *Store) Authenticate(ctx context.Context, secret string) (bool, error) {
	res, err := s.db.ExecContext(ctx,
		`UPDATE tokens SET last_used_at = ? WHERE hash = ? AND revoked = 0`,
		time.Now().Unix(), hashToken(secret))
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n == 1, err
}

func (s *Store) ListTokens(ctx context.Context) ([]Token, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, name, created_at, last_used_at, revoked FROM tokens ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Token
	for rows.Next() {
		var t Token
		var created int64
		var used sql.NullInt64
		if err := rows.Scan(&t.ID, &t.Name, &created, &used, &t.Revoked); err != nil {
			return nil, err
		}
		t.CreatedAt = time.Unix(created, 0)
		if used.Valid {
			u := time.Unix(used.Int64, 0)
			t.LastUsedAt = &u
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// RevokeToken revokes tokens matching idOrName (numeric id or name).
func (s *Store) RevokeToken(ctx context.Context, idOrName string) (int64, error) {
	res, err := s.db.ExecContext(ctx,
		`UPDATE tokens SET revoked = 1 WHERE (CAST(id AS TEXT) = ? OR name = ?) AND revoked = 0`,
		idOrName, idOrName)
	if err != nil {
		return 0, err
	}
	n, err := res.RowsAffected()
	if err == nil && n == 0 {
		err = ErrNotFound
	}
	return n, err
}
