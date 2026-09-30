package store

import (
	"context"
	"strings"
	"testing"

	"github.com/dvher/pogo_pad/internal/model"
)

func TestQuota(t *testing.T) {
	s, ctx := open(t), context.Background()
	u := mustUser(t, s, "alice")
	s.SetLimits(Limits{MaxNotes: 2, MaxBytes: 10})

	clock := int64(0)
	note := func(id, content string, deleted bool) model.Note {
		clock++
		return model.Note{ID: id, Content: content, Deleted: deleted, UpdatedAt: clock}
	}
	if _, err := s.Sync(ctx, u, 0, []model.Note{note("a", "12345", false), note("b", "123", false)}, 100); err != nil {
		t.Fatal(err)
	}
	// A third note breaks MaxNotes, and the whole batch is rejected.
	if _, err := s.Sync(ctx, u, 0, []model.Note{note("c", "1", false)}, 100); err != ErrQuota {
		t.Fatalf("third note: %v", err)
	}
	// Growing past MaxBytes is rejected.
	if _, err := s.Sync(ctx, u, 0, []model.Note{note("a", "123456789", false)}, 100); err != ErrQuota {
		t.Fatalf("too many bytes: %v", err)
	}
	if got, _ := s.Usage(ctx, u); got != (Usage{Notes: 2, Bytes: 8}) {
		t.Fatalf("usage after rejected syncs: %+v", got)
	}
	// Deleting and replacing in one batch stays within limits.
	if _, err := s.Sync(ctx, u, 0, []model.Note{note("b", "", true), note("c", "1234", false)}, 100); err != nil {
		t.Fatalf("swap: %v", err)
	}

	// Lowering the limits leaves the user over quota, but shrinking still works.
	s.SetLimits(Limits{MaxNotes: 1, MaxBytes: 1})
	if _, err := s.Sync(ctx, u, 0, []model.Note{note("c", "12", false)}, 100); err != nil {
		t.Fatalf("shrink while over quota: %v", err)
	}
	// Reading never hits the quota.
	if _, err := s.Sync(ctx, u, 0, nil, 100); err != nil {
		t.Fatalf("read while over quota: %v", err)
	}
}

func TestSignUp(t *testing.T) {
	s, ctx := open(t), context.Background()
	u, secret, err := s.SignUp(ctx, SignUp{Name: "alice", Email: "alice@example.com", PasswordHash: "h", Device: "laptop"})
	if err != nil {
		t.Fatal(err)
	}
	if a, ok, _ := s.Authenticate(ctx, secret); !ok || a.UserID != u.ID {
		t.Fatal("signup token rejected")
	}
	if _, _, err := s.SignUp(ctx, SignUp{Name: "alice", PasswordHash: "h", Device: "d"}); err != ErrNameTaken {
		t.Fatalf("taken name: %v", err)
	}
	if _, _, err := s.SignUp(ctx, SignUp{Name: "al", Email: "alice@example.com", PasswordHash: "h", Device: "d"}); err != ErrEmailTaken {
		t.Fatalf("taken email: %v", err)
	}
	// Several users may have no email.
	for _, n := range []string{"bob", "carol"} {
		if _, _, err := s.SignUp(ctx, SignUp{Name: n, PasswordHash: "h", Device: "d"}); err != nil {
			t.Fatalf("%s without email: %v", n, err)
		}
	}

	id, hash, err := s.Credentials(ctx, "ALICE@example.com")
	if err != nil || id != u.ID || hash != "h" {
		t.Fatalf("credentials by email: %d %q %v", id, hash, err)
	}
	if id, _, _ := s.Credentials(ctx, "alice"); id != u.ID {
		t.Fatal("credentials by name")
	}
	if _, _, err := s.Credentials(ctx, "nobody"); err != ErrNotFound {
		t.Fatalf("unknown login: %v", err)
	}
}

func TestInvites(t *testing.T) {
	s, ctx := open(t), context.Background()
	code, err := s.CreateInvite(ctx)
	if err != nil || !strings.HasPrefix(code, "inv_") {
		t.Fatalf("create invite: %q %v", code, err)
	}
	if _, _, err := s.SignUp(ctx, SignUp{Name: "eve", PasswordHash: "h", Invite: "inv_bogus", Device: "d"}); err != ErrInvalidInvite {
		t.Fatalf("bogus invite: %v", err)
	}
	if _, err := s.GetUser(ctx, "eve"); err != ErrNotFound {
		t.Fatal("user created despite a bad invite")
	}
	if _, _, err := s.SignUp(ctx, SignUp{Name: "alice", PasswordHash: "h", Invite: code, Device: "d"}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.SignUp(ctx, SignUp{Name: "bob", PasswordHash: "h", Invite: code, Device: "d"}); err != ErrInvalidInvite {
		t.Fatalf("reused invite: %v", err)
	}
	list, _ := s.ListInvites(ctx)
	if len(list) != 1 || list[0].UsedBy != "alice" || list[0].UsedAt == nil {
		t.Fatalf("list: %+v", list)
	}
	if err := s.DeleteInvite(ctx, list[0].ID); err != ErrNotFound {
		t.Fatalf("deleting a used invite: %v", err)
	}
	s.CreateInvite(ctx)
	list, _ = s.ListInvites(ctx)
	if err := s.DeleteInvite(ctx, list[1].ID); err != nil {
		t.Fatal(err)
	}
}

func TestAccountTokens(t *testing.T) {
	s, ctx := open(t), context.Background()
	alice, bob := mustUser(t, s, "alice"), mustUser(t, s, "bob")
	keep, _ := s.CreateToken(ctx, alice, "laptop")
	other, _ := s.CreateToken(ctx, alice, "phone")
	bobs, _ := s.CreateToken(ctx, bob, "phone")
	ka, _, _ := s.Authenticate(ctx, keep)
	ba, _, _ := s.Authenticate(ctx, bobs)

	if err := s.RevokeUserToken(ctx, alice, ba.TokenID); err != ErrNotFound {
		t.Fatalf("revoking another user's token: %v", err)
	}
	if n, err := s.RevokeOtherTokens(ctx, alice, ka.TokenID); err != nil || n != 1 {
		t.Fatalf("revoke others: %d %v", n, err)
	}
	for secret, want := range map[string]bool{keep: true, other: false, bobs: true} {
		if _, ok, _ := s.Authenticate(ctx, secret); ok != want {
			t.Errorf("token valid = %v, want %v", ok, want)
		}
	}

	if err := s.SetPassword(ctx, alice, "h"); err != nil {
		t.Fatal(err)
	}
	if h, _ := s.PasswordHash(ctx, alice); h != "h" {
		t.Fatalf("hash: %q", h)
	}
	s.SetPassword(ctx, alice, "")
	if u, _ := s.GetUser(ctx, "alice"); u.HasPassword {
		t.Fatal("password not cleared")
	}
	if err := s.SetEmail(ctx, alice, "A@x.org"); err != nil {
		t.Fatal(err)
	}
	if err := s.SetEmail(ctx, bob, "a@X.org"); err != ErrEmailTaken {
		t.Fatalf("email case clash: %v", err)
	}
}
