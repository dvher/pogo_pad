package api

import (
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"net/mail"
	"regexp"
	"strconv"
	"strings"

	"github.com/dvher/pogo_pad/internal/password"
	"github.com/dvher/pogo_pad/internal/store"
)

// Names chosen at signup are lowercase so they can't be confused with each
// other; the CLI is less strict.
var signupName = regexp.MustCompile(`^[a-z0-9][a-z0-9_.-]{2,31}$`)

const maxDeviceLen = 64

// decode reads a small JSON body into v, writing a 400 on failure.
func decode(w http.ResponseWriter, r *http.Request, v any) bool {
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10)).Decode(v); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
		return false
	}
	return true
}

// normEmail validates an optional email address and lowercases it.
func normEmail(email string) (string, error) {
	if email == "" {
		return "", nil
	}
	a, err := mail.ParseAddress(email)
	if err != nil || a.Address != email || len(email) > 254 {
		return "", errors.New("invalid email address")
	}
	return strings.ToLower(email), nil
}

func validDevice(name string) error {
	if name == "" || len(name) > maxDeviceLen {
		return errors.New("device must be 1-64 characters")
	}
	return nil
}

func internalError(w http.ResponseWriter, what string, err error) {
	log.Printf("%s: %v", what, err)
	writeError(w, http.StatusInternalServerError, "internal error")
}

type accountUser struct {
	Name  string `json:"name"`
	Email string `json:"email,omitempty"`
}

type tokenResponse struct {
	Token string      `json:"token"`
	User  accountUser `json:"user"`
}

func (s *Server) signup(w http.ResponseWriter, r *http.Request) {
	if s.cfg.Signup == SignupClosed {
		writeError(w, http.StatusForbidden, "signup is disabled on this server")
		return
	}
	var req struct {
		Name     string `json:"name"`
		Email    string `json:"email"`
		Password string `json:"password"`
		Invite   string `json:"invite"`
		Device   string `json:"device"`
	}
	if !decode(w, r, &req) || !s.throttle(w, r) {
		return
	}
	email, err := normEmail(req.Email)
	switch {
	case !signupName.MatchString(req.Name):
		err = errors.New("name must be 3-32 characters: lowercase letters, digits, '.', '_' or '-'")
	case err != nil:
	case s.cfg.Signup == SignupInvite && req.Invite == "":
		err = errors.New("an invite code is required")
	default:
		if err = password.Check(req.Password); err == nil {
			err = validDevice(req.Device)
		}
	}
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	hash, err := password.Hash(req.Password)
	if err != nil {
		internalError(w, "signup", err)
		return
	}
	invite := ""
	if s.cfg.Signup == SignupInvite {
		invite = req.Invite
	}
	u, secret, err := s.store.SignUp(r.Context(), store.SignUp{
		Name: req.Name, Email: email, PasswordHash: hash, Invite: invite, Device: req.Device,
	})
	switch {
	case errors.Is(err, store.ErrNameTaken), errors.Is(err, store.ErrEmailTaken):
		writeError(w, http.StatusConflict, err.Error())
	case errors.Is(err, store.ErrInvalidInvite):
		writeError(w, http.StatusForbidden, err.Error())
	case err != nil:
		internalError(w, "signup", err)
	default:
		writeJSON(w, http.StatusCreated, tokenResponse{Token: secret, User: accountUser{u.Name, u.Email}})
	}
}

func (s *Server) login(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Login    string `json:"login"` // user name or email
		Password string `json:"password"`
		Device   string `json:"device"`
	}
	if !decode(w, r, &req) {
		return
	}
	if err := validDevice(req.Device); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if len(req.Password) > password.MaxLen {
		writeError(w, http.StatusUnauthorized, "wrong user name or password")
		return
	}
	if !s.throttle(w, r) {
		return
	}
	uid, hash, err := s.store.Credentials(r.Context(), req.Login)
	if err != nil && !errors.Is(err, store.ErrNotFound) {
		internalError(w, "login", err)
		return
	}
	ok := false
	if hash == "" {
		// Unknown user, or one without a password: do the same work as a
		// real check so response times don't reveal which names exist.
		password.VerifyNothing(req.Password)
	} else if ok, err = password.Verify(req.Password, hash); err != nil {
		internalError(w, "login", err)
		return
	}
	if !ok {
		writeError(w, http.StatusUnauthorized, "wrong user name or password")
		return
	}
	secret, err := s.store.CreateToken(r.Context(), uid, req.Device)
	if err != nil {
		internalError(w, "login", err)
		return
	}
	u, err := s.store.GetUserByID(r.Context(), uid)
	if err != nil {
		internalError(w, "login", err)
		return
	}
	writeJSON(w, http.StatusOK, tokenResponse{Token: secret, User: accountUser{u.Name, u.Email}})
}

func (s *Server) logout(w http.ResponseWriter, r *http.Request) {
	a := authOf(r)
	if err := s.store.RevokeUserToken(r.Context(), a.UserID, a.TokenID); err != nil {
		internalError(w, "logout", err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) getAccount(w http.ResponseWriter, r *http.Request) {
	u, err := s.store.GetUserByID(r.Context(), userID(r))
	if err != nil {
		internalError(w, "account", err)
		return
	}
	lim := s.store.Limits()
	writeJSON(w, http.StatusOK, map[string]any{
		"name":         u.Name,
		"email":        u.Email,
		"has_password": u.HasPassword,
		"created_at":   u.CreatedAt.Unix(),
		"usage":        map[string]any{"notes": u.Notes, "bytes": u.Bytes},
		"limits":       map[string]any{"max_notes": lim.MaxNotes, "max_bytes": lim.MaxBytes},
	})
}

// checkPassword verifies the account password sent with a sensitive
// request, so a stolen device token alone can't take over the account.
// It writes the error response and returns false on failure.
func (s *Server) checkPassword(w http.ResponseWriter, r *http.Request, pw string) bool {
	if !s.throttle(w, r) {
		return false
	}
	hash, err := s.store.PasswordHash(r.Context(), userID(r))
	if err != nil {
		internalError(w, "password", err)
		return false
	}
	if hash == "" {
		writeError(w, http.StatusForbidden, "this account has no password; ask the server admin to set one")
		return false
	}
	ok, err := password.Verify(pw, hash)
	if err != nil {
		internalError(w, "password", err)
		return false
	}
	if !ok {
		writeError(w, http.StatusForbidden, "wrong password")
	}
	return ok
}

func (s *Server) putPassword(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Current            string `json:"current"`
		New                string `json:"new"`
		RevokeOtherDevices bool   `json:"revoke_other_devices"`
	}
	if !decode(w, r, &req) {
		return
	}
	if err := password.Check(req.New); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if !s.checkPassword(w, r, req.Current) {
		return
	}
	hash, err := password.Hash(req.New)
	if err == nil {
		err = s.store.SetPassword(r.Context(), userID(r), hash)
	}
	if err == nil && req.RevokeOtherDevices {
		a := authOf(r)
		_, err = s.store.RevokeOtherTokens(r.Context(), a.UserID, a.TokenID)
	}
	if err != nil {
		internalError(w, "password", err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) putEmail(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Email    string `json:"email"` // empty removes it
		Password string `json:"password"`
	}
	if !decode(w, r, &req) {
		return
	}
	email, err := normEmail(req.Email)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if !s.checkPassword(w, r, req.Password) {
		return
	}
	err = s.store.SetEmail(r.Context(), userID(r), email)
	if errors.Is(err, store.ErrEmailTaken) {
		writeError(w, http.StatusConflict, err.Error())
		return
	}
	if err != nil {
		internalError(w, "email", err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) deleteAccount(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Password string `json:"password"`
	}
	if !decode(w, r, &req) || !s.checkPassword(w, r, req.Password) {
		return
	}
	if err := s.store.DeleteUserByID(r.Context(), userID(r)); err != nil {
		internalError(w, "delete account", err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

type device struct {
	ID         int64  `json:"id"`
	Name       string `json:"name"`
	CreatedAt  int64  `json:"created_at"`
	LastUsedAt *int64 `json:"last_used_at"`
	Current    bool   `json:"current"` // the token making this request
}

func (s *Server) listDevices(w http.ResponseWriter, r *http.Request) {
	a := authOf(r)
	tokens, err := s.store.ListTokens(r.Context(), a.UserID)
	if err != nil {
		internalError(w, "devices", err)
		return
	}
	out := []device{}
	for _, t := range tokens {
		if t.Revoked {
			continue
		}
		d := device{ID: t.ID, Name: t.Name, CreatedAt: t.CreatedAt.Unix(), Current: t.ID == a.TokenID}
		if t.LastUsedAt != nil {
			d.LastUsedAt = new(t.LastUsedAt.Unix())
		}
		out = append(out, d)
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) deleteDevice(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		writeError(w, http.StatusNotFound, "no such device")
		return
	}
	err = s.store.RevokeUserToken(r.Context(), userID(r), id)
	if errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusNotFound, "no such device")
		return
	}
	if err != nil {
		internalError(w, "devices", err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
