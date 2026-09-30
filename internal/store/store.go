// Package store persists users, their notes and their API tokens in SQLite.
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
	"strings"
	"time"

	_ "modernc.org/sqlite"

	"github.com/dvher/pogo_pad/internal/lww"
	"github.com/dvher/pogo_pad/internal/model"
)

var (
	// ErrNotFound is returned when a user or token lookup matches nothing.
	ErrNotFound = errors.New("not found")
	// ErrAmbiguous is returned by RevokeToken when a token name matches
	// tokens of more than one user.
	ErrAmbiguous = errors.New("matches tokens of more than one user")
	// ErrQuota is returned by Sync when the changes would take a user over
	// one of the store's Limits.
	ErrQuota = errors.New("quota exceeded")
)

type Store struct {
	db     *sql.DB
	limits Limits
}

// Limits caps what each user may store. Zero means unlimited.
type Limits struct {
	MaxNotes int   // notes that are not deleted
	MaxBytes int64 // total content bytes of notes that are not deleted
}

// SetLimits sets the per-user quotas enforced by Sync.
func (s *Store) SetLimits(l Limits) { s.limits = l }

// Limits returns the per-user quotas.
func (s *Store) Limits() Limits { return s.limits }

// Usage is what a user currently stores.
type Usage struct {
	Notes int
	Bytes int64
}

func usage(ctx context.Context, q interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}, userID int64) (Usage, error) {
	var u Usage
	err := q.QueryRowContext(ctx, `
		SELECT COUNT(*), COALESCE(SUM(LENGTH(CAST(content AS BLOB))), 0)
		FROM notes WHERE user_id = ? AND deleted = 0`, userID).Scan(&u.Notes, &u.Bytes)
	return u, err
}

// Usage returns what userID currently stores.
func (s *Store) Usage(ctx context.Context, userID int64) (Usage, error) {
	return usage(ctx, s.db, userID)
}

// migrations[i] upgrades the schema from version i to i+1; the version is
// stored in PRAGMA user_version. Version 0 is the original single-user
// schema, which had no users table.
var migrations = []func(*sql.Tx) error{migrateMultiUser, migrateAccounts}

// schemaV1 is the multi-user schema.
const schemaV1 = `
CREATE TABLE IF NOT EXISTS users (
	id         INTEGER PRIMARY KEY AUTOINCREMENT,
	name       TEXT    NOT NULL UNIQUE,
	created_at INTEGER NOT NULL
);
CREATE TABLE IF NOT EXISTS notes (
	user_id    INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
	id         TEXT    NOT NULL,
	content    TEXT    NOT NULL DEFAULT '',
	color      TEXT    NOT NULL DEFAULT '',
	deleted    INTEGER NOT NULL DEFAULT 0,
	updated_at INTEGER NOT NULL,
	device_id  TEXT    NOT NULL DEFAULT '',
	rev        INTEGER NOT NULL,
	PRIMARY KEY (user_id, id)
);
CREATE UNIQUE INDEX IF NOT EXISTS notes_user_rev ON notes(user_id, rev);
CREATE TABLE IF NOT EXISTS meta (
	user_id INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
	key     TEXT    NOT NULL,
	value   TEXT    NOT NULL,
	PRIMARY KEY (user_id, key)
);
CREATE TABLE IF NOT EXISTS tokens (
	id           INTEGER PRIMARY KEY AUTOINCREMENT,
	user_id      INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
	name         TEXT    NOT NULL,
	hash         TEXT    NOT NULL UNIQUE,
	created_at   INTEGER NOT NULL,
	last_used_at INTEGER,
	revoked      INTEGER NOT NULL DEFAULT 0
);
CREATE INDEX IF NOT EXISTS tokens_user ON tokens(user_id);
`

// schemaV2 adds HTTP accounts: an optional email and password per user,
// and single-use invite codes for invite-only signup.
const schemaV2 = `
ALTER TABLE users ADD COLUMN email TEXT;
ALTER TABLE users ADD COLUMN password_hash TEXT;
CREATE UNIQUE INDEX users_email ON users(email);
CREATE TABLE invites (
	id         INTEGER PRIMARY KEY AUTOINCREMENT,
	hash       TEXT    NOT NULL UNIQUE,
	created_at INTEGER NOT NULL,
	used_by    INTEGER REFERENCES users(id) ON DELETE SET NULL,
	used_at    INTEGER
);
`

// LegacyUser is the user that owns the data of a database created before
// multi-user support.
const LegacyUser = "default"

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
	if err := migrate(db); err != nil {
		db.Close()
		return nil, fmt.Errorf("migrate: %w", err)
	}
	return &Store{db: db}, nil
}

// migrate brings the schema up to the latest version, one step at a time.
func migrate(db *sql.DB) error {
	var ver int
	if err := db.QueryRow(`PRAGMA user_version`).Scan(&ver); err != nil {
		return err
	}
	for ; ver < len(migrations); ver++ {
		tx, err := db.Begin()
		if err != nil {
			return err
		}
		if err := migrations[ver](tx); err != nil {
			tx.Rollback()
			return fmt.Errorf("to version %d: %w", ver+1, err)
		}
		if _, err := tx.Exec(fmt.Sprintf(`PRAGMA user_version = %d`, ver+1)); err != nil {
			tx.Rollback()
			return err
		}
		if err := tx.Commit(); err != nil {
			return err
		}
	}
	return nil
}

// migrateMultiUser creates the multi-user schema. A database that already
// has tables is single-user: its notes, meta and tokens are moved to
// LegacyUser, keeping token ids and note revs so existing clients carry on.
func migrateMultiUser(tx *sql.Tx) error {
	var legacy bool
	if err := tx.QueryRow(
		`SELECT EXISTS (SELECT 1 FROM sqlite_master WHERE type = 'table' AND name = 'notes')`,
	).Scan(&legacy); err != nil {
		return err
	}
	if legacy {
		for _, t := range []string{"notes", "meta", "tokens"} {
			if _, err := tx.Exec(`ALTER TABLE ` + t + ` RENAME TO ` + t + `_v0`); err != nil {
				return err
			}
		}
	}
	if _, err := tx.Exec(schemaV1); err != nil {
		return err
	}
	if !legacy {
		return nil
	}
	var hasData bool
	if err := tx.QueryRow(`SELECT EXISTS (SELECT 1 FROM notes_v0) OR EXISTS (SELECT 1 FROM meta_v0)
		OR EXISTS (SELECT 1 FROM tokens_v0)`).Scan(&hasData); err != nil {
		return err
	}
	if hasData {
		var uid int64
		if err := tx.QueryRow(`INSERT INTO users (name, created_at) VALUES (?, ?) RETURNING id`,
			LegacyUser, time.Now().Unix()).Scan(&uid); err != nil {
			return err
		}
		for _, q := range []string{
			`INSERT INTO notes (user_id, id, content, color, deleted, updated_at, device_id, rev)
				SELECT ?, id, content, color, deleted, updated_at, device_id, rev FROM notes_v0`,
			`INSERT INTO meta (user_id, key, value) SELECT ?, key, value FROM meta_v0`,
			`INSERT INTO tokens (id, user_id, name, hash, created_at, last_used_at, revoked)
				SELECT id, ?, name, hash, created_at, last_used_at, revoked FROM tokens_v0`,
		} {
			if _, err := tx.Exec(q, uid); err != nil {
				return err
			}
		}
	}
	for _, t := range []string{"notes_v0", "meta_v0", "tokens_v0"} {
		if _, err := tx.Exec(`DROP TABLE ` + t); err != nil {
			return err
		}
	}
	return nil
}

func migrateAccounts(tx *sql.Tx) error {
	_, err := tx.Exec(schemaV2)
	return err
}

func (s *Store) Close() error { return s.db.Close() }

// Sync applies incoming changes to userID's notes with last-write-wins and
// returns up to limit of their notes whose rev is greater than cursor. Revs
// are allocated per user. If the changes would leave the user over the
// store's Limits, and above where they started, nothing is applied and
// ErrQuota is returned; changes that only shrink usage always go through.
func (s *Store) Sync(ctx context.Context, userID, cursor int64, changes []model.Note, limit int) (model.SyncResponse, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return model.SyncResponse{}, err
	}
	defer tx.Rollback()

	var maxRev int64
	if err := tx.QueryRowContext(ctx, `SELECT COALESCE(MAX(rev), 0) FROM notes WHERE user_id = ?`, userID).Scan(&maxRev); err != nil {
		return model.SyncResponse{}, err
	}
	limited := len(changes) > 0 && (s.limits.MaxNotes > 0 || s.limits.MaxBytes > 0)
	var before Usage
	if limited {
		if before, err = usage(ctx, tx, userID); err != nil {
			return model.SyncResponse{}, err
		}
	}

	for _, in := range changes {
		var cur model.Note
		err := tx.QueryRowContext(ctx,
			`SELECT updated_at, device_id FROM notes WHERE user_id = ? AND id = ?`, userID, in.ID,
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
			INSERT INTO notes (user_id, id, content, color, deleted, updated_at, device_id, rev)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?)
			ON CONFLICT(user_id, id) DO UPDATE SET
				content = excluded.content, color = excluded.color, deleted = excluded.deleted,
				updated_at = excluded.updated_at, device_id = excluded.device_id, rev = excluded.rev`,
			userID, in.ID, in.Content, in.Color, in.Deleted, in.UpdatedAt, in.DeviceID, maxRev,
		); err != nil {
			return model.SyncResponse{}, err
		}
	}

	if limited {
		after, err := usage(ctx, tx, userID)
		if err != nil {
			return model.SyncResponse{}, err
		}
		if s.limits.MaxNotes > 0 && after.Notes > s.limits.MaxNotes && after.Notes > before.Notes ||
			s.limits.MaxBytes > 0 && after.Bytes > s.limits.MaxBytes && after.Bytes > before.Bytes {
			return model.SyncResponse{}, ErrQuota
		}
	}

	rows, err := tx.QueryContext(ctx, `
		SELECT id, content, color, deleted, updated_at, device_id, rev
		FROM notes WHERE user_id = ? AND rev > ? ORDER BY rev LIMIT ?`, userID, cursor, limit+1)
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

// GetMeta returns the opaque value stored under key for userID.
func (s *Store) GetMeta(ctx context.Context, userID int64, key string) (string, error) {
	var v string
	err := s.db.QueryRowContext(ctx, `SELECT value FROM meta WHERE user_id = ? AND key = ?`, userID, key).Scan(&v)
	if errors.Is(err, sql.ErrNoRows) {
		return "", ErrNotFound
	}
	return v, err
}

// SetMeta stores value under key for userID. Unless overwrite is true it
// fails with ErrExists when the key already has a value.
func (s *Store) SetMeta(ctx context.Context, userID int64, key, value string, overwrite bool) error {
	q := `INSERT INTO meta (user_id, key, value) VALUES (?, ?, ?) ON CONFLICT(user_id, key) DO NOTHING`
	if overwrite {
		q = `INSERT INTO meta (user_id, key, value) VALUES (?, ?, ?)
			ON CONFLICT(user_id, key) DO UPDATE SET value = excluded.value`
	}
	res, err := s.db.ExecContext(ctx, q, userID, key, value)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrExists
	}
	return nil
}

// DeleteMeta removes key for userID.
func (s *Store) DeleteMeta(ctx context.Context, userID int64, key string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM meta WHERE user_id = ? AND key = ?`, userID, key)
	return err
}

// User is an account whose notes are kept apart from every other user's.
type User struct {
	ID          int64
	Name        string
	Email       string // empty if none
	HasPassword bool   // can sign in over HTTP
	CreatedAt   time.Time
	Tokens      int // active (non-revoked) tokens
	Usage
}

// CreateUser adds a user. It fails with ErrExists if the name is taken.
func (s *Store) CreateUser(ctx context.Context, name string) (User, error) {
	u := User{Name: name, CreatedAt: time.Unix(time.Now().Unix(), 0)}
	err := s.db.QueryRowContext(ctx,
		`INSERT INTO users (name, created_at) VALUES (?, ?) ON CONFLICT(name) DO NOTHING RETURNING id`,
		name, u.CreatedAt.Unix()).Scan(&u.ID)
	if errors.Is(err, sql.ErrNoRows) {
		return User{}, ErrExists
	}
	return u, err
}

// GetUser looks a user up by name.
func (s *Store) GetUser(ctx context.Context, name string) (User, error) {
	return s.getUser(ctx, `WHERE u.name = ?`, name)
}

// GetUserByID looks a user up by id.
func (s *Store) GetUserByID(ctx context.Context, id int64) (User, error) {
	return s.getUser(ctx, `WHERE u.id = ?`, id)
}

func (s *Store) getUser(ctx context.Context, where string, arg any) (User, error) {
	users, err := s.listUsers(ctx, where, arg)
	if err != nil {
		return User{}, err
	}
	if len(users) == 0 {
		return User{}, ErrNotFound
	}
	return users[0], nil
}

// ListUsers returns all users with their token and note counts.
func (s *Store) ListUsers(ctx context.Context) ([]User, error) {
	return s.listUsers(ctx, "")
}

func (s *Store) listUsers(ctx context.Context, where string, args ...any) ([]User, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT u.id, u.name, COALESCE(u.email, ''), u.password_hash IS NOT NULL, u.created_at,
			(SELECT COUNT(*) FROM tokens t WHERE t.user_id = u.id AND t.revoked = 0),
			(SELECT COUNT(*) FROM notes n WHERE n.user_id = u.id AND n.deleted = 0),
			(SELECT COALESCE(SUM(LENGTH(CAST(n.content AS BLOB))), 0) FROM notes n
				WHERE n.user_id = u.id AND n.deleted = 0)
		FROM users u `+where+` ORDER BY u.id`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []User
	for rows.Next() {
		var u User
		var created int64
		if err := rows.Scan(&u.ID, &u.Name, &u.Email, &u.HasPassword, &created, &u.Tokens, &u.Notes, &u.Bytes); err != nil {
			return nil, err
		}
		u.CreatedAt = time.Unix(created, 0)
		out = append(out, u)
	}
	return out, rows.Err()
}

// RenameUser renames a user. Tokens keep working.
func (s *Store) RenameUser(ctx context.Context, oldName, newName string) error {
	res, err := s.db.ExecContext(ctx, `UPDATE users SET name = ? WHERE name = ?`, newName, oldName)
	if err != nil {
		if strings.Contains(err.Error(), "UNIQUE constraint failed") {
			return ErrExists
		}
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// DeleteUser removes a user together with all their notes, settings and tokens.
func (s *Store) DeleteUser(ctx context.Context, name string) error {
	res, err := s.db.ExecContext(ctx, `DELETE FROM users WHERE name = ?`, name)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// DeleteUserByID removes a user together with all their notes, settings and
// tokens.
func (s *Store) DeleteUserByID(ctx context.Context, id int64) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM users WHERE id = ?`, id)
	return err
}

// Token is an API token record (the secret itself is never stored).
type Token struct {
	ID         int64
	User       string
	Name       string
	CreatedAt  time.Time
	LastUsedAt *time.Time
	Revoked    bool
}

func hashToken(secret string) string {
	sum := sha256.Sum256([]byte(secret))
	return hex.EncodeToString(sum[:])
}

// CreateToken generates a new token called name for userID and returns the
// plaintext secret.
func (s *Store) CreateToken(ctx context.Context, userID int64, name string) (string, error) {
	return createToken(ctx, s.db, userID, name)
}

type execer interface {
	ExecContext(context.Context, string, ...any) (sql.Result, error)
}

func newSecret(prefix string) (string, error) {
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return prefix + base64.RawURLEncoding.EncodeToString(buf), nil
}

func createToken(ctx context.Context, db execer, userID int64, name string) (string, error) {
	secret, err := newSecret("pogo_")
	if err != nil {
		return "", err
	}
	_, err = db.ExecContext(ctx,
		`INSERT INTO tokens (user_id, name, hash, created_at) VALUES (?, ?, ?, ?)`,
		userID, name, hashToken(secret), time.Now().Unix())
	return secret, err
}

// Auth identifies the token a request was made with.
type Auth struct {
	UserID  int64
	TokenID int64
}

// Authenticate checks that secret is a valid, non-revoked token, records its
// use and returns the token and the user it belongs to. ok is false for an
// unknown or revoked token.
func (s *Store) Authenticate(ctx context.Context, secret string) (a Auth, ok bool, err error) {
	err = s.db.QueryRowContext(ctx,
		`UPDATE tokens SET last_used_at = ? WHERE hash = ? AND revoked = 0 RETURNING user_id, id`,
		time.Now().Unix(), hashToken(secret)).Scan(&a.UserID, &a.TokenID)
	if errors.Is(err, sql.ErrNoRows) {
		return Auth{}, false, nil
	}
	return a, err == nil, err
}

// ListTokens returns the tokens of userID, or of every user when userID is 0.
func (s *Store) ListTokens(ctx context.Context, userID int64) ([]Token, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT t.id, u.name, t.name, t.created_at, t.last_used_at, t.revoked
		FROM tokens t JOIN users u ON u.id = t.user_id
		WHERE ? = 0 OR t.user_id = ?
		ORDER BY t.id`, userID, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Token
	for rows.Next() {
		var t Token
		var created int64
		var used sql.NullInt64
		if err := rows.Scan(&t.ID, &t.User, &t.Name, &created, &used, &t.Revoked); err != nil {
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

// RevokeToken revokes active tokens matching idOrName (numeric id or name),
// limited to userID unless it is 0. Without a user, a name that matches
// tokens of several users fails with ErrAmbiguous.
func (s *Store) RevokeToken(ctx context.Context, userID int64, idOrName string) (int64, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()

	var users int
	if err := tx.QueryRowContext(ctx, `
		SELECT COUNT(DISTINCT user_id) FROM tokens
		WHERE (CAST(id AS TEXT) = ? OR name = ?) AND revoked = 0 AND (? = 0 OR user_id = ?)`,
		idOrName, idOrName, userID, userID).Scan(&users); err != nil {
		return 0, err
	}
	switch {
	case users == 0:
		return 0, ErrNotFound
	case users > 1:
		return 0, ErrAmbiguous
	}
	res, err := tx.ExecContext(ctx, `
		UPDATE tokens SET revoked = 1
		WHERE (CAST(id AS TEXT) = ? OR name = ?) AND revoked = 0 AND (? = 0 OR user_id = ?)`,
		idOrName, idOrName, userID, userID)
	if err != nil {
		return 0, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, err
	}
	return n, tx.Commit()
}
