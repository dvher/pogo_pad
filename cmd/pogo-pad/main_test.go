package main

import (
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestParseSize(t *testing.T) {
	for in, want := range map[string]int64{
		"0": 0, "1048576": 1 << 20, "512KB": 512 << 10, "50mb": 50 << 20,
		" 2 GB ": 2 << 30, "7B": 7,
	} {
		if got, err := parseSize(in); err != nil || got != want {
			t.Errorf("parseSize(%q) = %d, %v; want %d", in, got, err, want)
		}
	}
	for _, in := range []string{"", "MB", "-1", "1.5GB", "ten", "99999999999GB"} {
		if got, err := parseSize(in); err == nil {
			t.Errorf("parseSize(%q) = %d, want an error", in, got)
		}
	}
}

func TestFormatSize(t *testing.T) {
	for n, want := range map[int64]string{
		0: "0 B", 1023: "1023 B", 1 << 10: "1.0 KB", 1536 << 10: "1.5 MB", 3 << 30: "3.0 GB",
	} {
		if got := formatSize(n); got != want {
			t.Errorf("formatSize(%d) = %q, want %q", n, got, want)
		}
	}
}

func TestValidUserName(t *testing.T) {
	for _, ok := range []string{"a", "Alice", "bob.smith-2", strings.Repeat("x", maxUserLen)} {
		if err := validUserName(ok); err != nil {
			t.Errorf("validUserName(%q): %v", ok, err)
		}
	}
	for _, bad := range []string{"", "two words", "tab\there", strings.Repeat("x", maxUserLen+1)} {
		if validUserName(bad) == nil {
			t.Errorf("validUserName(%q) accepted", bad)
		}
	}
}

// The tests below run the real binary, built once by TestMain.
var bin string

func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "pogo-pad-cli")
	if err != nil {
		panic(err)
	}
	bin = filepath.Join(dir, "pogo-pad")
	out, err := exec.Command("go", "build", "-ldflags", "-X main.version=v9.9.9-test", "-o", bin, ".").CombinedOutput()
	if err != nil {
		fmt.Fprintf(os.Stderr, "build: %v\n%s", err, out)
		os.Exit(1)
	}
	code := m.Run()
	os.RemoveAll(dir)
	os.Exit(code)
}

type result struct {
	stdout, stderr string
	code           int
}

// run runs the binary against db with stdin, from a clean environment.
func run(t *testing.T, db, stdin string, args ...string) result {
	t.Helper()
	cmd := exec.Command(bin, args...)
	cmd.Env = []string{"POGO_DB=" + db, "PATH=" + os.Getenv("PATH")}
	cmd.Stdin = strings.NewReader(stdin)
	var stdout, stderr strings.Builder
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	err := cmd.Run()
	r := result{stdout.String(), stderr.String(), 0}
	var exit *exec.ExitError
	if errors.As(err, &exit) {
		r.code = exit.ExitCode()
	} else if err != nil {
		t.Fatal(err)
	}
	return r
}

// ok runs a command that must succeed and returns its stdout.
func ok(t *testing.T, db string, args ...string) string {
	t.Helper()
	r := run(t, db, "", args...)
	if r.code != 0 {
		t.Fatalf("%v: exit %d\n%s", args, r.code, r.stderr)
	}
	return r.stdout
}

// fails runs a command that must fail with an error mentioning want.
func fails(t *testing.T, db, want string, args ...string) {
	t.Helper()
	r := run(t, db, "", args...)
	if r.code == 0 || !strings.Contains(r.stderr, want) {
		t.Fatalf("%v: exit %d, stderr %q; want a failure mentioning %q", args, r.code, r.stderr, want)
	}
}

func TestCLIBasics(t *testing.T) {
	db := filepath.Join(t.TempDir(), "pad.db")
	if got := ok(t, db, "version"); got != "v9.9.9-test\n" {
		t.Errorf("version: %q", got)
	}
	if got := ok(t, db, "help"); !strings.Contains(got, "Usage:") {
		t.Errorf("help: %q", got)
	}
	for _, args := range [][]string{{}, {"bogus"}} {
		if r := run(t, db, "", args...); r.code != 2 || !strings.Contains(r.stderr, "Usage:") {
			t.Errorf("%v: exit %d, stderr %q", args, r.code, r.stderr)
		}
	}
	fails(t, db, "expected create", "user")
	fails(t, db, "unknown subcommand", "user", "frobnicate")
	fails(t, db, "expected 1 argument", "user", "create")
	fails(t, db, "expected create", "token")
	fails(t, db, "unknown subcommand", "token", "frobnicate")
	fails(t, db, "expected create", "invite")
	fails(t, db, "unknown subcommand", "invite", "frobnicate")
	fails(t, db, "--signup must be", "serve", "--signup", "sometimes")
	fails(t, db, "--max-storage", "serve", "--max-storage", "lots")
	fails(t, db, "--max-notes", "serve", "--max-notes", "-1")
}

func TestCLIUsersAndTokens(t *testing.T) {
	db := filepath.Join(t.TempDir(), "pad.db")

	fails(t, db, "--name is required", "token", "create")

	// On an empty database, token create makes the "default" user.
	r := run(t, db, "", "token", "create", "--name", "laptop")
	if r.code != 0 || !strings.HasPrefix(r.stdout, "pogo_") || !strings.Contains(r.stderr, `Created user "default"`) {
		t.Fatalf("first token: %+v", r)
	}
	// With one user, --user can be left out.
	if r := run(t, db, "", "token", "create", "--name", "phone"); r.code != 0 || strings.Contains(r.stderr, "Created user") {
		t.Fatalf("second token: %+v", r)
	}

	ok(t, db, "user", "create", "--email", "alice@example.com", "alice")
	fails(t, db, "already exists", "user", "create", "alice")
	fails(t, db, "without spaces", "user", "create", "al ice")
	fails(t, db, "invalid email", "user", "create", "--email", "not an email", "carol")
	fails(t, db, "already used", "user", "create", "--email", "alice@example.com", "carol")
	// A failed create leaves no half-made user behind.
	if got := ok(t, db, "user", "list"); strings.Contains(got, "carol") {
		t.Errorf("carol was created:\n%s", got)
	}
	fails(t, db, "several users", "token", "create", "--name", "tablet")

	// Passwords come from stdin when it is not a terminal.
	if r := run(t, db, "short\n", "user", "create", "--password", "bob"); r.code == 0 {
		t.Fatal("accepted a too-short password")
	}
	if r := run(t, db, "hunter22!\n", "user", "create", "--password", "bob"); r.code != 0 {
		t.Fatalf("user create --password: %+v", r)
	}
	if r := run(t, db, "", "user", "passwd", "bob"); r.code == 0 {
		t.Fatal("passwd with empty stdin succeeded")
	}

	list := ok(t, db, "user", "list")
	for _, want := range []string{"alice@example.com", "bob", "default"} {
		if !strings.Contains(list, want) {
			t.Errorf("user list lacks %q:\n%s", want, list)
		}
	}
	if !strings.Contains(lineWith(list, "bob"), "yes") || !strings.Contains(lineWith(list, "alice"), "no") {
		t.Errorf("user list password column:\n%s", list)
	}

	ok(t, db, "user", "rename", "alice", "alicia")
	fails(t, db, "already exists", "user", "rename", "alicia", "bob")
	fails(t, db, "no user", "user", "rename", "nobody", "somebody")
	fails(t, db, "without spaces", "user", "rename", "alicia", "a b")

	fails(t, db, "already used", "user", "email", "bob", "alice@example.com")
	ok(t, db, "user", "email", "alicia", "")
	ok(t, db, "user", "email", "bob", "alice@example.com")
	fails(t, db, "no user", "user", "email", "nobody", "x@example.com")

	ok(t, db, "user", "passwd", "--clear", "bob")
	if !strings.Contains(lineWith(ok(t, db, "user", "list"), "bob"), " no ") {
		t.Error("bob still has a password after passwd --clear")
	}

	tokens := ok(t, db, "token", "list", "--user", "default")
	if !strings.Contains(tokens, "laptop") || !strings.Contains(tokens, "phone") {
		t.Fatalf("token list:\n%s", tokens)
	}
	fails(t, db, "no user", "token", "list", "--user", "nobody")
	fails(t, db, "exactly one", "token", "revoke")
	if got := ok(t, db, "token", "revoke", "laptop"); got != "revoked 1 token(s)\n" {
		t.Errorf("revoke: %q", got)
	}
	fails(t, db, "no active token", "token", "revoke", "laptop")
	if !strings.Contains(lineWith(ok(t, db, "token", "list"), "laptop"), "revoked") {
		t.Error("laptop not listed as revoked")
	}

	// The same device name for two users needs --user.
	ok(t, db, "token", "create", "--user", "bob", "--name", "phone")
	fails(t, db, "pass --user", "token", "revoke", "phone")
	ok(t, db, "token", "revoke", "--user", "bob", "phone")

	ok(t, db, "user", "delete", "default")
	fails(t, db, "no user", "user", "delete", "default")
	if strings.Contains(ok(t, db, "token", "list"), "default") {
		t.Error("deleted user's tokens are still listed")
	}
}

func TestCLIInvites(t *testing.T) {
	db := filepath.Join(t.TempDir(), "pad.db")
	code := strings.TrimSpace(ok(t, db, "invite", "create"))
	if !strings.HasPrefix(code, "inv_") {
		t.Fatalf("invite code %q", code)
	}
	if got := ok(t, db, "invite", "list"); !strings.Contains(lineWith(got, "1"), "unused") {
		t.Fatalf("invite list:\n%s", got)
	}
	fails(t, db, "exactly one ID", "invite", "delete")
	fails(t, db, "not an ID", "invite", "delete", "first")
	fails(t, db, "no unused invite", "invite", "delete", "42")
	ok(t, db, "invite", "delete", "1")
	if got := ok(t, db, "invite", "list"); strings.Contains(got, "unused") {
		t.Fatalf("invite still listed:\n%s", got)
	}
}

// TestServe starts the server as users run it, syncs a note, signs in with a
// password set through the CLI and shuts it down with SIGTERM.
func TestServe(t *testing.T) {
	db := filepath.Join(t.TempDir(), "pad.db")
	token := strings.TrimSpace(ok(t, db, "token", "create", "--name", "laptop"))
	if r := run(t, db, "hunter22!\n", "user", "passwd", "default"); r.code != 0 {
		t.Fatalf("passwd: %+v", r)
	}

	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := l.Addr().String()
	l.Close()
	cmd := exec.Command(bin, "serve", "--addr", addr, "--max-storage", "1MB", "--max-notes", "10")
	cmd.Env = []string{"POGO_DB=" + db}
	var logs strings.Builder
	cmd.Stderr = &logs
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	t.Cleanup(func() { cmd.Process.Kill() })

	base := "http://" + addr + "/api/v1"
	for i := 0; ; i++ {
		resp, err := http.Get(base + "/health")
		if err == nil {
			resp.Body.Close()
			break
		}
		if i == 100 {
			t.Fatalf("server did not start: %v\n%s", err, logs.String())
		}
		time.Sleep(50 * time.Millisecond)
	}

	post := func(path, token, body string) int {
		req, _ := http.NewRequest("POST", base+path, strings.NewReader(body))
		if token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		return resp.StatusCode
	}
	if got := post("/sync", token, `{"changes":[{"id":"n1","content":"hi","updated_at":1,"device_id":"d"}]}`); got != 200 {
		t.Errorf("sync: %d", got)
	}
	if got := post("/login", "", `{"login":"default","password":"hunter22!","device":"phone"}`); got != 200 {
		t.Errorf("login with the CLI-set password: %d", got)
	}
	if got := post("/signup", "", `{"name":"eve","password":"hunter22!","device":"x"}`); got != 403 {
		t.Errorf("signup is closed by default, got %d", got)
	}

	cmd.Process.Signal(syscall.SIGTERM)
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("serve exited with %v\n%s", err, logs.String())
		}
	case <-time.After(10 * time.Second):
		t.Fatal("serve did not stop on SIGTERM")
	}
	if !strings.Contains(logs.String(), "listening on "+addr) {
		t.Errorf("logs:\n%s", logs.String())
	}
}

// lineWith returns the first line of s containing sub.
func lineWith(s, sub string) string {
	for line := range strings.Lines(s) {
		if strings.Contains(line, sub) {
			return line
		}
	}
	return ""
}
