package api

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/dvher/pogo_pad/internal/store"
)

type client struct {
	t   *testing.T
	url string
}

// do sends body (JSON) with token and decodes the response into out if given.
func (c client) do(token, method, path, body string, out any) int {
	c.t.Helper()
	req, _ := http.NewRequest(method, c.url+path, strings.NewReader(body))
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		c.t.Fatal(err)
	}
	defer resp.Body.Close()
	if out != nil {
		json.NewDecoder(resp.Body).Decode(out)
	} else {
		io.Copy(io.Discard, resp.Body)
	}
	return resp.StatusCode
}

func newTestServer(t *testing.T, cfg Config) (*store.Store, client) {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "acct.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	srv := httptest.NewServer(New(st, cfg).Handler())
	t.Cleanup(srv.Close)
	return st, client{t, srv.URL}
}

func TestSignupModes(t *testing.T) {
	body := `{"name":"alice","password":"hunter22!","device":"laptop"}`

	_, c := newTestServer(t, Config{})
	var health map[string]any
	c.do("", "GET", "/api/v1/health", "", &health)
	if health["signup"] != "closed" {
		t.Fatalf("health: %v", health)
	}
	if got := c.do("", "POST", "/api/v1/signup", body, nil); got != 403 {
		t.Fatalf("closed signup: %d", got)
	}

	st, c := newTestServer(t, Config{Signup: SignupInvite})
	if got := c.do("", "POST", "/api/v1/signup", body, nil); got != 400 {
		t.Fatalf("invite signup without code: %d", got)
	}
	code, _ := st.CreateInvite(context.Background())
	withCode := `{"name":"alice","password":"hunter22!","device":"laptop","invite":"` + code + `"}`
	if got := c.do("", "POST", "/api/v1/signup", withCode, nil); got != 201 {
		t.Fatalf("invite signup: %d", got)
	}
	if got := c.do("", "POST", "/api/v1/signup", strings.Replace(withCode, "alice", "bob", 1), nil); got != 403 {
		t.Fatalf("reused invite: %d", got)
	}

	_, c = newTestServer(t, Config{Signup: SignupOpen})
	for _, bad := range []string{
		`{"name":"Alice","password":"hunter22!","device":"d"}`,
		`{"name":"al","password":"hunter22!","device":"d"}`,
		`{"name":"alice","password":"short","device":"d"}`,
		`{"name":"alice","password":"hunter22!","device":""}`,
		`{"name":"alice","email":"not an email","password":"hunter22!","device":"d"}`,
	} {
		if got := c.do("", "POST", "/api/v1/signup", bad, nil); got != 400 {
			t.Errorf("%s: %d", bad, got)
		}
	}
	var resp tokenResponse
	if got := c.do("", "POST", "/api/v1/signup",
		`{"name":"alice","email":"Alice@Example.com","password":"hunter22!","device":"laptop"}`, &resp); got != 201 {
		t.Fatalf("open signup: %d", got)
	}
	if resp.User.Email != "alice@example.com" || !strings.HasPrefix(resp.Token, "pogo_") {
		t.Fatalf("signup response: %+v", resp)
	}
	if got := c.do(resp.Token, "POST", "/api/v1/sync", `{}`, nil); got != 200 {
		t.Fatalf("sync with signup token: %d", got)
	}
	if got := c.do("", "POST", "/api/v1/signup", `{"name":"alice","password":"hunter22!","device":"d"}`, nil); got != 409 {
		t.Fatalf("taken name: %d", got)
	}
}

func TestLoginAndAccount(t *testing.T) {
	_, c := newTestServer(t, Config{Signup: SignupOpen})
	var first tokenResponse
	c.do("", "POST", "/api/v1/signup", `{"name":"alice","email":"alice@example.com","password":"hunter22!","device":"laptop"}`, &first)

	if got := c.do("", "POST", "/api/v1/login", `{"login":"alice","password":"wrong-password","device":"phone"}`, nil); got != 401 {
		t.Fatalf("wrong password: %d", got)
	}
	if got := c.do("", "POST", "/api/v1/login", `{"login":"nobody","password":"hunter22!","device":"phone"}`, nil); got != 401 {
		t.Fatalf("unknown user: %d", got)
	}
	var phone tokenResponse
	if got := c.do("", "POST", "/api/v1/login", `{"login":"ALICE@example.com","password":"hunter22!","device":"phone"}`, &phone); got != 200 {
		t.Fatalf("login by email: %d", got)
	}
	if phone.User.Name != "alice" || phone.Token == first.Token {
		t.Fatalf("login response: %+v", phone)
	}

	var acct map[string]any
	c.do(phone.Token, "GET", "/api/v1/account", "", &acct)
	if acct["name"] != "alice" || acct["has_password"] != true {
		t.Fatalf("account: %v", acct)
	}

	var devices []device
	c.do(phone.Token, "GET", "/api/v1/devices", "", &devices)
	if len(devices) != 2 || devices[0].Name != "laptop" || devices[0].Current || !devices[1].Current {
		t.Fatalf("devices: %+v", devices)
	}

	// Sensitive changes need the password.
	if got := c.do(phone.Token, "PUT", "/api/v1/account/email", `{"email":"new@example.com","password":"nope-nope"}`, nil); got != 403 {
		t.Fatalf("email with wrong password: %d", got)
	}
	if got := c.do(phone.Token, "PUT", "/api/v1/account/email", `{"email":"new@example.com","password":"hunter22!"}`, nil); got != 204 {
		t.Fatalf("email: %d", got)
	}
	if got := c.do(phone.Token, "PUT", "/api/v1/account/password",
		`{"current":"hunter22!","new":"correct horse","revoke_other_devices":true}`, nil); got != 204 {
		t.Fatalf("password: %d", got)
	}
	if got := c.do(first.Token, "GET", "/api/v1/account", "", nil); got != 401 {
		t.Fatalf("other device still signed in: %d", got)
	}
	if got := c.do("", "POST", "/api/v1/login", `{"login":"new@example.com","password":"correct horse","device":"tablet"}`, nil); got != 200 {
		t.Fatalf("login with new password and email: %d", got)
	}

	// Revoke the tablet by id, then log the phone out.
	c.do(phone.Token, "GET", "/api/v1/devices", "", &devices)
	tablet := devices[len(devices)-1]
	if got := c.do(phone.Token, "DELETE", "/api/v1/devices/"+strconv.FormatInt(tablet.ID, 10), "", nil); got != 204 {
		t.Fatalf("revoke device: %d", got)
	}
	if got := c.do(phone.Token, "DELETE", "/api/v1/devices/"+strconv.FormatInt(tablet.ID, 10), "", nil); got != 404 {
		t.Fatalf("revoke twice: %d", got)
	}
	if got := c.do(phone.Token, "POST", "/api/v1/logout", "", nil); got != 204 {
		t.Fatalf("logout: %d", got)
	}
	if got := c.do(phone.Token, "GET", "/api/v1/account", "", nil); got != 401 {
		t.Fatalf("token works after logout: %d", got)
	}
}

func TestAccountIsolationAndDelete(t *testing.T) {
	st, c := newTestServer(t, Config{Signup: SignupOpen})
	var alice, bob tokenResponse
	c.do("", "POST", "/api/v1/signup", `{"name":"alice","password":"hunter22!","device":"d"}`, &alice)
	c.do("", "POST", "/api/v1/signup", `{"name":"bob","password":"hunter22!","device":"d"}`, &bob)

	var bobDevices []device
	c.do(bob.Token, "GET", "/api/v1/devices", "", &bobDevices)
	if got := c.do(alice.Token, "DELETE", "/api/v1/devices/"+strconv.FormatInt(bobDevices[0].ID, 10), "", nil); got != 404 {
		t.Fatalf("alice revoked bob's device: %d", got)
	}

	// A user made on the CLI has no password, so can't use password-guarded endpoints.
	u, _ := st.CreateUser(context.Background(), "carol")
	carol, _ := st.CreateToken(context.Background(), u.ID, "d")
	if got := c.do(carol, "DELETE", "/api/v1/account", `{"password":""}`, nil); got != 403 {
		t.Fatalf("delete without a password set: %d", got)
	}

	if got := c.do(alice.Token, "DELETE", "/api/v1/account", `{"password":"hunter22!"}`, nil); got != 204 {
		t.Fatalf("delete account: %d", got)
	}
	if got := c.do(alice.Token, "POST", "/api/v1/sync", `{}`, nil); got != 401 {
		t.Fatalf("deleted account's token: %d", got)
	}
	if got := c.do(bob.Token, "POST", "/api/v1/sync", `{}`, nil); got != 200 {
		t.Fatalf("bob after alice left: %d", got)
	}
	// The name is free again.
	if got := c.do("", "POST", "/api/v1/signup", `{"name":"alice","password":"hunter22!","device":"d"}`, nil); got != 201 {
		t.Fatalf("re-signup: %d", got)
	}
}

func TestQuotaStatus(t *testing.T) {
	st, c := newTestServer(t, Config{})
	st.SetLimits(store.Limits{MaxNotes: 1})
	u, _ := st.CreateUser(context.Background(), "alice")
	tok, _ := st.CreateToken(context.Background(), u.ID, "d")
	two := `{"changes":[{"id":"a","updated_at":1},{"id":"b","updated_at":1}]}`
	var e map[string]string
	if got := c.do(tok, "POST", "/api/v1/sync", two, &e); got != 507 || !strings.Contains(e["error"], "quota") {
		t.Fatalf("over quota: %d %v", got, e)
	}
	var acct struct {
		Limits map[string]int `json:"limits"`
	}
	c.do(tok, "GET", "/api/v1/account", "", &acct)
	if acct.Limits["max_notes"] != 1 {
		t.Fatalf("account limits: %+v", acct)
	}
}

func TestRateLimit(t *testing.T) {
	_, c := newTestServer(t, Config{})
	wrong := `{"login":"x","password":"wrong-password","device":"d"}`
	for i := range 10 {
		if got := c.do("", "POST", "/api/v1/login", wrong, nil); got != 401 {
			t.Fatalf("attempt %d: %d", i, got)
		}
	}
	if got := c.do("", "POST", "/api/v1/login", wrong, nil); got != 429 {
		t.Fatalf("11th attempt: %d", got)
	}
}

func TestLimiter(t *testing.T) {
	now := time.Unix(0, 0)
	l := newLimiter(2, time.Minute)
	l.now = func() time.Time { return now }
	if !l.allow("a") || !l.allow("a") || l.allow("a") {
		t.Fatal("burst")
	}
	if !l.allow("b") {
		t.Fatal("addresses share a bucket")
	}
	now = now.Add(30 * time.Second) // one token back
	if !l.allow("a") || l.allow("a") {
		t.Fatal("refill")
	}
}

func TestClientAddr(t *testing.T) {
	r := httptest.NewRequest("GET", "/", nil)
	r.RemoteAddr = "10.0.0.1:1234"
	r.Header.Set("X-Forwarded-For", "6.6.6.6, 203.0.113.9")
	if got := (&Server{}).clientAddr(r); got != "10.0.0.1" {
		t.Errorf("untrusted proxy: %s", got)
	}
	if got := (&Server{cfg: Config{TrustProxy: true}}).clientAddr(r); got != "203.0.113.9" {
		t.Errorf("trusted proxy: %s", got)
	}
}
