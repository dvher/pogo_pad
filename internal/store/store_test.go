package store

import (
	"context"
	"path/filepath"
	"testing"

	"notes-server/internal/model"
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

func TestSyncLastWriteWins(t *testing.T) {
	s, ctx := open(t), context.Background()

	resp, err := s.Sync(ctx, 0, []model.Note{{ID: "n1", Content: "v1", UpdatedAt: 100, DeviceID: "a"}}, 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(resp.Changes) != 1 || resp.Cursor != 1 {
		t.Fatalf("first sync: %+v", resp)
	}

	// Older write is rejected and produces no new rev.
	resp, _ = s.Sync(ctx, 1, []model.Note{{ID: "n1", Content: "old", UpdatedAt: 50, DeviceID: "b"}}, 100)
	if len(resp.Changes) != 0 || resp.Cursor != 1 {
		t.Fatalf("older write accepted: %+v", resp)
	}

	// Newer write wins.
	resp, _ = s.Sync(ctx, 1, []model.Note{{ID: "n1", Content: "v2", UpdatedAt: 200, DeviceID: "b"}}, 100)
	if len(resp.Changes) != 1 || resp.Changes[0].Content != "v2" || resp.Cursor != 2 {
		t.Fatalf("newer write: %+v", resp)
	}

	// Tombstone propagates.
	resp, _ = s.Sync(ctx, 2, []model.Note{{ID: "n1", Deleted: true, UpdatedAt: 300, DeviceID: "a"}}, 100)
	if len(resp.Changes) != 1 || !resp.Changes[0].Deleted {
		t.Fatalf("tombstone: %+v", resp)
	}
}

func TestSyncPaging(t *testing.T) {
	s, ctx := open(t), context.Background()
	var changes []model.Note
	for i := range 5 {
		changes = append(changes, model.Note{ID: string(rune('a' + i)), UpdatedAt: 1})
	}
	if _, err := s.Sync(ctx, 0, changes, 100); err != nil {
		t.Fatal(err)
	}

	var got []string
	cursor := int64(0)
	for {
		resp, err := s.Sync(ctx, cursor, nil, 2)
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

func TestTokens(t *testing.T) {
	s, ctx := open(t), context.Background()
	secret, err := s.CreateToken(ctx, "laptop")
	if err != nil {
		t.Fatal(err)
	}
	if ok, _ := s.Authenticate(ctx, secret); !ok {
		t.Fatal("valid token rejected")
	}
	if ok, _ := s.Authenticate(ctx, "nts_bogus"); ok {
		t.Fatal("bogus token accepted")
	}
	if _, err := s.RevokeToken(ctx, "laptop"); err != nil {
		t.Fatal(err)
	}
	if ok, _ := s.Authenticate(ctx, secret); ok {
		t.Fatal("revoked token accepted")
	}
	if _, err := s.RevokeToken(ctx, "missing"); err != ErrNotFound {
		t.Fatalf("revoke missing: %v", err)
	}
}
