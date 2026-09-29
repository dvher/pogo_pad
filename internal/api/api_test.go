package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"notes-server/internal/model"
	"notes-server/internal/store"
)

func TestSyncEndpoint(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "api.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	secret, _ := st.CreateToken(context.Background(), "test")
	srv := httptest.NewServer(New(st, "test").Handler())
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
	if r := post("nts_wrong", `{}`); r.StatusCode != 401 {
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

	h, _ := http.Get(srv.URL + "/api/v1/health")
	if h.StatusCode != 200 {
		t.Errorf("health: %d", h.StatusCode)
	}
}
