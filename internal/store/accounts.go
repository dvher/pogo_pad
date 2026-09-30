package store

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"time"
)

var (
	// ErrNameTaken and ErrEmailTaken are returned when creating or
	// changing an account would clash with another user.
	ErrNameTaken  = errors.New("user name is taken")
	ErrEmailTaken = errors.New("email is already registered")
	// ErrInvalidInvite is returned by SignUp for an unknown or used invite.
	ErrInvalidInvite = errors.New("invalid or used invite code")
)

// uniqueErr maps a UNIQUE constraint failure on users to ErrNameTaken or
// ErrEmailTaken.
func uniqueErr(err error) error {
	if err == nil {
		return nil
	}
	msg := err.Error()
	switch {
	case strings.Contains(msg, "UNIQUE constraint failed: users.name"):
		return ErrNameTaken
	case strings.Contains(msg, "UNIQUE constraint failed: users.email"):
		return ErrEmailTaken
	}
	return err
}

// SignUp is what's needed to create an account over HTTP.
type SignUp struct {
	Name         string
	Email        string // optional
	PasswordHash string
	Invite       string // required when non-empty; checked and consumed
	Device       string // name of the first device token
}

// SignUp creates a user with a password and a first device token, in one
// transaction, and returns the token's secret. If an invite is given it
// must be unused, and it is marked as used by the new user.
func (s *Store) SignUp(ctx context.Context, p SignUp) (User, string, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return User{}, "", err
	}
	defer tx.Rollback()

	now := time.Now().Unix()
	u := User{Name: p.Name, Email: p.Email, HasPassword: true, CreatedAt: time.Unix(now, 0)}
	err = tx.QueryRowContext(ctx,
		`INSERT INTO users (name, email, password_hash, created_at) VALUES (?, NULLIF(?, ''), ?, ?) RETURNING id`,
		p.Name, p.Email, p.PasswordHash, now).Scan(&u.ID)
	if err != nil {
		return User{}, "", uniqueErr(err)
	}
	if p.Invite != "" {
		res, err := tx.ExecContext(ctx,
			`UPDATE invites SET used_by = ?, used_at = ? WHERE hash = ? AND used_at IS NULL`,
			u.ID, now, hashToken(p.Invite))
		if err != nil {
			return User{}, "", err
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return User{}, "", ErrInvalidInvite
		}
	}
	secret, err := createToken(ctx, tx, u.ID, p.Device)
	if err != nil {
		return User{}, "", err
	}
	u.Tokens = 1
	return u, secret, tx.Commit()
}

// Credentials returns the id and password hash of the user whose name, or
// email if login contains "@", is login. The hash is empty if the user has
// no password.
func (s *Store) Credentials(ctx context.Context, login string) (id int64, hash string, err error) {
	q := `SELECT id, COALESCE(password_hash, '') FROM users WHERE name = ?`
	if strings.Contains(login, "@") {
		q = `SELECT id, COALESCE(password_hash, '') FROM users WHERE email = ?`
		login = strings.ToLower(login)
	}
	err = s.db.QueryRowContext(ctx, q, login).Scan(&id, &hash)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, "", ErrNotFound
	}
	return id, hash, err
}

// PasswordHash returns userID's password hash, or "" if they have none.
func (s *Store) PasswordHash(ctx context.Context, userID int64) (string, error) {
	var h sql.NullString
	err := s.db.QueryRowContext(ctx, `SELECT password_hash FROM users WHERE id = ?`, userID).Scan(&h)
	if errors.Is(err, sql.ErrNoRows) {
		return "", ErrNotFound
	}
	return h.String, err
}

// SetPassword sets userID's password hash; an empty hash removes the
// password, so the user can no longer sign in over HTTP.
func (s *Store) SetPassword(ctx context.Context, userID int64, hash string) error {
	res, err := s.db.ExecContext(ctx,
		`UPDATE users SET password_hash = NULLIF(?, '') WHERE id = ?`, hash, userID)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// SetEmail sets userID's email; an empty email removes it.
func (s *Store) SetEmail(ctx context.Context, userID int64, email string) error {
	res, err := s.db.ExecContext(ctx,
		`UPDATE users SET email = NULLIF(?, '') WHERE id = ?`, strings.ToLower(email), userID)
	if err != nil {
		return uniqueErr(err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// RevokeUserToken revokes one of userID's active tokens by id.
func (s *Store) RevokeUserToken(ctx context.Context, userID, tokenID int64) error {
	res, err := s.db.ExecContext(ctx,
		`UPDATE tokens SET revoked = 1 WHERE id = ? AND user_id = ? AND revoked = 0`, tokenID, userID)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// RevokeOtherTokens revokes all of userID's tokens except keep and returns
// how many were revoked.
func (s *Store) RevokeOtherTokens(ctx context.Context, userID, keep int64) (int64, error) {
	res, err := s.db.ExecContext(ctx,
		`UPDATE tokens SET revoked = 1 WHERE user_id = ? AND id != ? AND revoked = 0`, userID, keep)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// Invite is a single-use signup code (the code itself is never stored).
type Invite struct {
	ID        int64
	CreatedAt time.Time
	UsedBy    string // user name; empty while unused or if that user was deleted
	UsedAt    *time.Time
}

// CreateInvite makes a new invite and returns its code.
func (s *Store) CreateInvite(ctx context.Context) (string, error) {
	code, err := newSecret("inv_")
	if err != nil {
		return "", err
	}
	_, err = s.db.ExecContext(ctx,
		`INSERT INTO invites (hash, created_at) VALUES (?, ?)`, hashToken(code), time.Now().Unix())
	return code, err
}

// ListInvites returns all invites, oldest first.
func (s *Store) ListInvites(ctx context.Context) ([]Invite, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT i.id, i.created_at, COALESCE(u.name, ''), i.used_at
		FROM invites i LEFT JOIN users u ON u.id = i.used_by ORDER BY i.id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Invite
	for rows.Next() {
		var inv Invite
		var created int64
		var used sql.NullInt64
		if err := rows.Scan(&inv.ID, &created, &inv.UsedBy, &used); err != nil {
			return nil, err
		}
		inv.CreatedAt = time.Unix(created, 0)
		if used.Valid {
			t := time.Unix(used.Int64, 0)
			inv.UsedAt = &t
		}
		out = append(out, inv)
	}
	return out, rows.Err()
}

// DeleteInvite removes an unused invite.
func (s *Store) DeleteInvite(ctx context.Context, id int64) error {
	res, err := s.db.ExecContext(ctx, `DELETE FROM invites WHERE id = ? AND used_at IS NULL`, id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}
