package store

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"

	"github.com/dvher/pogo_pad/internal/model"
)

func open(t *testing.T) *Store {
	t.Helper()
	s, err := Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func mustUser(t *testing.T, s *Store, name string) int64 {
	t.Helper()
	u, err := s.CreateUser(context.Background(), name)
	if err != nil {
		t.Fatal(err)
	}
	return u.ID
}

func TestSyncLastWriteWins(t *testing.T) {
	s, ctx := open(t), context.Background()
	u := mustUser(t, s, "alice")

	resp, err := s.Sync(ctx, u, 0, []model.Note{{ID: "n1", Content: "v1", UpdatedAt: 100, DeviceID: "a"}}, 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(resp.Changes) != 1 || resp.Cursor != 1 {
		t.Fatalf("first sync: %+v", resp)
	}

	// Older write is rejected and produces no new rev.
	resp, _ = s.Sync(ctx, u, 1, []model.Note{{ID: "n1", Content: "old", UpdatedAt: 50, DeviceID: "b"}}, 100)
	if len(resp.Changes) != 0 || resp.Cursor != 1 {
		t.Fatalf("older write accepted: %+v", resp)
	}

	// Newer write wins.
	resp, _ = s.Sync(ctx, u, 1, []model.Note{{ID: "n1", Content: "v2", UpdatedAt: 200, DeviceID: "b"}}, 100)
	if len(resp.Changes) != 1 || resp.Changes[0].Content != "v2" || resp.Cursor != 2 {
		t.Fatalf("newer write: %+v", resp)
	}

	// Tombstone propagates.
	resp, _ = s.Sync(ctx, u, 2, []model.Note{{ID: "n1", Deleted: true, UpdatedAt: 300, DeviceID: "a"}}, 100)
	if len(resp.Changes) != 1 || !resp.Changes[0].Deleted {
		t.Fatalf("tombstone: %+v", resp)
	}
}

func TestSyncPaging(t *testing.T) {
	s, ctx := open(t), context.Background()
	u := mustUser(t, s, "alice")
	var changes []model.Note
	for i := range 5 {
		changes = append(changes, model.Note{ID: string(rune('a' + i)), UpdatedAt: 1})
	}
	if _, err := s.Sync(ctx, u, 0, changes, 100); err != nil {
		t.Fatal(err)
	}

	var got []string
	cursor := int64(0)
	for {
		resp, err := s.Sync(ctx, u, cursor, nil, 2)
		if err != nil {
			t.Fatal(err)
		}
		for _, n := range resp.Changes {
			got = append(got, n.ID)
		}
		cursor = resp.Cursor
		if !resp.More {
			break
		}
	}
	if len(got) != 5 || cursor != 5 {
		t.Fatalf("paging got %v cursor %d", got, cursor)
	}
}

func TestUsersAreIsolated(t *testing.T) {
	s, ctx := open(t), context.Background()
	alice, bob := mustUser(t, s, "alice"), mustUser(t, s, "bob")

	// Same note id for both users: neither overwrites the other.
	if _, err := s.Sync(ctx, alice, 0, []model.Note{{ID: "n1", Content: "alice", UpdatedAt: 100}}, 100); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Sync(ctx, alice, 0, []model.Note{{ID: "n2", Content: "alice 2", UpdatedAt: 100}}, 100); err != nil {
		t.Fatal(err)
	}
	resp, err := s.Sync(ctx, bob, 0, []model.Note{{ID: "n1", Content: "bob", UpdatedAt: 50}}, 100)
	if err != nil {
		t.Fatal(err)
	}
	// Bob's older write still lands, and his revs start at 1.
	if len(resp.Changes) != 1 || resp.Changes[0].Content != "bob" || resp.Cursor != 1 {
		t.Fatalf("bob: %+v", resp)
	}
	resp, _ = s.Sync(ctx, alice, 0, nil, 100)
	if len(resp.Changes) != 2 || resp.Changes[0].Content != "alice" || resp.Cursor != 2 {
		t.Fatalf("alice: %+v", resp)
	}

	if err := s.SetMeta(ctx, alice, "e2e", "a", false); err != nil {
		t.Fatal(err)
	}
	if _, err := s.GetMeta(ctx, bob, "e2e"); err != ErrNotFound {
		t.Fatalf("bob sees alice's meta: %v", err)
	}
	if err := s.SetMeta(ctx, bob, "e2e", "b", false); err != nil {
		t.Fatalf("bob set meta: %v", err)
	}
}

func TestUsers(t *testing.T) {
	s, ctx := open(t), context.Background()
	alice := mustUser(t, s, "alice")
	if _, err := s.CreateUser(ctx, "alice"); err != ErrExists {
		t.Fatalf("duplicate user: %v", err)
	}
	mustUser(t, s, "bob")
	if err := s.RenameUser(ctx, "alice", "bob"); err != ErrExists {
		t.Fatalf("rename onto existing: %v", err)
	}
	if err := s.RenameUser(ctx, "alice", "carol"); err != nil {
		t.Fatal(err)
	}
	if err := s.RenameUser(ctx, "alice", "dave"); err != ErrNotFound {
		t.Fatalf("rename missing: %v", err)
	}

	secret, _ := s.CreateToken(ctx, alice, "laptop")
	s.Sync(ctx, alice, 0, []model.Note{{ID: "n1", UpdatedAt: 1}}, 100)
	s.SetMeta(ctx, alice, "e2e", "x", false)
	if u, err := s.GetUser(ctx, "carol"); err != nil || u.Tokens != 1 || u.Notes != 1 {
		t.Fatalf("get user: %+v %v", u, err)
	}

	if err := s.DeleteUser(ctx, "carol"); err != nil {
		t.Fatal(err)
	}
	if _, ok, _ := s.Authenticate(ctx, secret); ok {
		t.Fatal("token of deleted user accepted")
	}
	var left int
	s.db.QueryRow(`SELECT (SELECT COUNT(*) FROM notes) + (SELECT COUNT(*) FROM meta) + (SELECT COUNT(*) FROM tokens)`).Scan(&left)
	if left != 0 {
		t.Fatalf("%d rows left after deleting user", left)
	}
	if err := s.DeleteUser(ctx, "carol"); err != ErrNotFound {
		t.Fatalf("delete missing: %v", err)
	}
}

func TestTokens(t *testing.T) {
	s, ctx := open(t), context.Background()
	alice, bob := mustUser(t, s, "alice"), mustUser(t, s, "bob")
	secret, err := s.CreateToken(ctx, alice, "laptop")
	if err != nil {
		t.Fatal(err)
	}
	bobSecret, _ := s.CreateToken(ctx, bob, "laptop")
	if a, ok, _ := s.Authenticate(ctx, secret); !ok || a.UserID != alice || a.TokenID != 1 {
		t.Fatalf("valid token: %+v ok %v", a, ok)
	}
	if a, ok, _ := s.Authenticate(ctx, bobSecret); !ok || a.UserID != bob {
		t.Fatalf("bob's token: %+v ok %v", a, ok)
	}
	if _, ok, _ := s.Authenticate(ctx, "pogo_bogus"); ok {
		t.Fatal("bogus token accepted")
	}
	if list, _ := s.ListTokens(ctx, bob); len(list) != 1 || list[0].User != "bob" {
		t.Fatalf("list bob's tokens: %+v", list)
	}
	if list, _ := s.ListTokens(ctx, 0); len(list) != 2 {
		t.Fatalf("list all tokens: %+v", list)
	}

	// "laptop" names a token of both users.
	if _, err := s.RevokeToken(ctx, 0, "laptop"); err != ErrAmbiguous {
		t.Fatalf("ambiguous revoke: %v", err)
	}
	if _, err := s.RevokeToken(ctx, alice, "laptop"); err != nil {
		t.Fatal(err)
	}
	if _, ok, _ := s.Authenticate(ctx, secret); ok {
		t.Fatal("revoked token accepted")
	}
	if _, ok, _ := s.Authenticate(ctx, bobSecret); !ok {
		t.Fatal("bob's token revoked along with alice's")
	}
	if _, err := s.RevokeToken(ctx, 0, "missing"); err != ErrNotFound {
		t.Fatalf("revoke missing: %v", err)
	}
}

// legacySchema is the single-user schema from before users existed.
const legacySchema = `
CREATE TABLE notes (id TEXT PRIMARY KEY, content TEXT NOT NULL DEFAULT '', color TEXT NOT NULL DEFAULT '',
	deleted INTEGER NOT NULL DEFAULT 0, updated_at INTEGER NOT NULL, device_id TEXT NOT NULL DEFAULT '',
	rev INTEGER NOT NULL);
CREATE INDEX notes_rev ON notes(rev);
CREATE TABLE meta (key TEXT PRIMARY KEY, value TEXT NOT NULL);
CREATE TABLE tokens (id INTEGER PRIMARY KEY AUTOINCREMENT, name TEXT NOT NULL, hash TEXT NOT NULL UNIQUE,
	created_at INTEGER NOT NULL, last_used_at INTEGER, revoked INTEGER NOT NULL DEFAULT 0);
`

func TestMigrateLegacy(t *testing.T) {
	path := filepath.Join(t.TempDir(), "legacy.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	secret := "pogo_legacy"
	for _, q := range []string{
		legacySchema,
		`INSERT INTO notes (id, content, updated_at, device_id, rev) VALUES ('n1', 'one', 10, 'd', 7), ('n2', 'two', 20, 'd', 9)`,
		`INSERT INTO meta (key, value) VALUES ('e2e', '{"salt":"s"}')`,
		`INSERT INTO tokens (id, name, hash, created_at) VALUES (5, 'laptop', '` + hashToken(secret) + `', 1)`,
	} {
		if _, err := db.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	db.Close()

	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	ctx := context.Background()

	a, ok, err := s.Authenticate(ctx, secret)
	uid := a.UserID
	if err != nil || !ok {
		t.Fatalf("legacy token: ok %v err %v", ok, err)
	}
	u, err := s.GetUser(ctx, LegacyUser)
	if err != nil || u.ID != uid || u.Notes != 2 {
		t.Fatalf("legacy user: %+v %v", u, err)
	}
	if list, _ := s.ListTokens(ctx, uid); len(list) != 1 || list[0].ID != 5 {
		t.Fatalf("token id not kept: %+v", list)
	}
	if v, err := s.GetMeta(ctx, uid, "e2e"); err != nil || v != `{"salt":"s"}` {
		t.Fatalf("meta: %q %v", v, err)
	}
	// Revs are kept, so a client that had synced up to rev 7 gets only n2,
	// and new writes continue after the highest rev.
	resp, err := s.Sync(ctx, uid, 7, []model.Note{{ID: "n3", UpdatedAt: 30}}, 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(resp.Changes) != 2 || resp.Changes[0].ID != "n2" || resp.Cursor != 10 {
		t.Fatalf("sync after migrate: %+v", resp)
	}
	s.Close()

	// Reopening a migrated database is a no-op.
	s2, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s2.Close()
	if users, _ := s2.ListUsers(ctx); len(users) != 1 {
		t.Fatalf("users after reopen: %+v", users)
	}
}

func TestMigrateEmptyLegacy(t *testing.T) {
	path := filepath.Join(t.TempDir(), "legacy.db")
	db, _ := sql.Open("sqlite", path)
	if _, err := db.Exec(legacySchema); err != nil {
		t.Fatal(err)
	}
	db.Close()
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if users, _ := s.ListUsers(context.Background()); len(users) != 0 {
		t.Fatalf("empty legacy db got users: %+v", users)
	}
}
