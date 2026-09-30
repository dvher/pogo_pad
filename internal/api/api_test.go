package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dvher/pogo_pad/internal/model"
	"github.com/dvher/pogo_pad/internal/store"
)

func TestSyncEndpoint(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "api.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	u, _ := st.CreateUser(context.Background(), "alice")
	secret, _ := st.CreateToken(context.Background(), u.ID, "test")
	srv := httptest.NewServer(New(st, Config{Version: "test"}).Handler())
	defer srv.Close()

	post := func(token, body string) *http.Response {
		req, _ := http.NewRequest("POST", srv.URL+"/api/v1/sync", strings.NewReader(body))
		if token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		return resp
	}

	if r := post("", `{}`); r.StatusCode != 401 {
		t.Errorf("no token: %d", r.StatusCode)
	}
	if r := post("pogo_wrong", `{}`); r.StatusCode != 401 {
		t.Errorf("bad token: %d", r.StatusCode)
	}
	if r := post(secret, `{"changes":[{"id":""}]}`); r.StatusCode != 400 {
		t.Errorf("invalid change: %d", r.StatusCode)
	}

	r := post(secret, `{"cursor":0,"changes":[{"id":"n1","content":"# hi","updated_at":1,"device_id":"d"}]}`)
	if r.StatusCode != 200 {
		t.Fatalf("sync: %d", r.StatusCode)
	}
	var resp model.SyncResponse
	json.NewDecoder(r.Body).Decode(&resp)
	if resp.Cursor != 1 || len(resp.Changes) != 1 || resp.Changes[0].Content != "# hi" {
		t.Fatalf("sync resp: %+v", resp)
	}

	// Another user's token sees none of alice's notes.
	bob, _ := st.CreateUser(context.Background(), "bob")
	bobSecret, _ := st.CreateToken(context.Background(), bob.ID, "phone")
	r = post(bobSecret, `{"cursor":0,"changes":[]}`)
	resp = model.SyncResponse{}
	json.NewDecoder(r.Body).Decode(&resp)
	if r.StatusCode != 200 || resp.Cursor != 0 || len(resp.Changes) != 0 {
		t.Fatalf("bob sync: %d %+v", r.StatusCode, resp)
	}

	h, _ := http.Get(srv.URL + "/api/v1/health")
	if h.StatusCode != 200 {
		t.Errorf("health: %d", h.StatusCode)
	}
}

func TestE2EEndpoints(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "e2e.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	ctx := context.Background()
	alice, _ := st.CreateUser(ctx, "alice")
	bob, _ := st.CreateUser(ctx, "bob")
	secret, _ := st.CreateToken(ctx, alice.ID, "test")
	bobSecret, _ := st.CreateToken(ctx, bob.ID, "test")
	srv := httptest.NewServer(New(st, Config{Version: "test"}).Handler())
	defer srv.Close()

	doAs := func(token, method, path, body string) int {
		req, _ := http.NewRequest(method, srv.URL+path, strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer "+token)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		return resp.StatusCode
	}
	do := func(method, path, body string) int { return doAs(secret, method, path, body) }
	params := `{"kdf":"argon2id","salt":"c2FsdA","check":"Y2hr"}`
	steps := []struct {
		method, path, body string
		want               int
	}{
		{"GET", "/api/v1/e2e", "", 404},
		{"PUT", "/api/v1/e2e", params, 200},
		{"GET", "/api/v1/e2e", "", 200},
		{"PUT", "/api/v1/e2e", params, 409},
		{"PUT", "/api/v1/e2e?force=1", params, 200},
		{"DELETE", "/api/v1/e2e", "", 204},
		{"GET", "/api/v1/e2e", "", 404},
	}
	for i, s := range steps {
		if got := do(s.method, s.path, s.body); got != s.want {
			t.Errorf("step %d %s %s: got %d want %d", i, s.method, s.path, got, s.want)
		}
	}

	// Each user has their own e2e setup.
	do("PUT", "/api/v1/e2e", params)
	if got := doAs(bobSecret, "GET", "/api/v1/e2e", ""); got != 404 {
		t.Errorf("bob sees alice's e2e: %d", got)
	}
	if got := doAs(bobSecret, "PUT", "/api/v1/e2e", params); got != 200 {
		t.Errorf("bob put e2e: %d", got)
	}
	do("DELETE", "/api/v1/e2e", "")
	if got := doAs(bobSecret, "GET", "/api/v1/e2e", ""); got != 200 {
		t.Errorf("alice's delete removed bob's e2e: %d", got)
	}
}
