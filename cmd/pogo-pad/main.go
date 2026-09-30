// Command pogo-pad is the self-hosted sync server for Pogo sticky notes.
package main

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"net/http"
	"net/mail"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"text/tabwriter"
	"time"
	"unicode"

	"golang.org/x/term"

	"github.com/dvher/pogo_pad/internal/api"
	"github.com/dvher/pogo_pad/internal/password"
	"github.com/dvher/pogo_pad/internal/store"
)

var version = "dev"

const usage = `pogo-pad — self-hosted sync server for Pogo

Usage:
  pogo-pad serve [--addr :8080] [--db PATH] [--tls-cert FILE --tls-key FILE]
                 [--signup closed|invite|open] [--max-notes N] [--max-storage SIZE]
                 [--trust-proxy]
  pogo-pad user create [--db PATH] [--email EMAIL] [--password] NAME
  pogo-pad user list [--db PATH]
  pogo-pad user rename [--db PATH] OLD NEW
  pogo-pad user email [--db PATH] NAME EMAIL      (EMAIL "" removes it)
  pogo-pad user passwd [--db PATH] [--clear] NAME
  pogo-pad user delete [--db PATH] NAME
  pogo-pad invite create|list [--db PATH]
  pogo-pad invite delete [--db PATH] ID
  pogo-pad token create [--db PATH] [--user USER] --name NAME
  pogo-pad token list [--db PATH] [--user USER]
  pogo-pad token revoke [--db PATH] [--user USER] ID|NAME
  pogo-pad version

Each user has their own notes and end-to-end encryption settings; a token
only reaches its user's data. --user may be left out while there is only one
user (on an empty database, token create makes a user called "default").

Users with a password can also sign in from the apps, which creates a token
for the device. --password and passwd read the password from the terminal,
or from the first line of stdin when it is not a terminal.

serve --signup lets people create accounts over HTTP: "invite" needs a code
from invite create. --max-storage takes bytes or a size such as 50MB.

The database path defaults to $POGO_DB or ./data/pogo-pad.db.
`

func main() {
	log.SetFlags(log.LstdFlags)
	if len(os.Args) < 2 {
		fmt.Fprint(os.Stderr, usage)
		os.Exit(2)
	}
	var err error
	switch os.Args[1] {
	case "serve":
		err = serve(os.Args[2:])
	case "user":
		err = user(os.Args[2:])
	case "token":
		err = token(os.Args[2:])
	case "invite":
		err = invite(os.Args[2:])
	case "version":
		fmt.Println(version)
	case "-h", "--help", "help":
		fmt.Print(usage)
	default:
		fmt.Fprint(os.Stderr, usage)
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

func defaultDB() string {
	if p := os.Getenv("POGO_DB"); p != "" {
		return p
	}
	return "./data/pogo-pad.db"
}

func serve(args []string) error {
	fs := flag.NewFlagSet("serve", flag.ExitOnError)
	addr := fs.String("addr", envOr("POGO_ADDR", ":8080"), "listen address")
	dbPath := fs.String("db", defaultDB(), "SQLite database path")
	cert := fs.String("tls-cert", os.Getenv("POGO_TLS_CERT"), "TLS certificate file (optional)")
	key := fs.String("tls-key", os.Getenv("POGO_TLS_KEY"), "TLS key file (optional)")
	signup := fs.String("signup", envOr("POGO_SIGNUP", api.SignupClosed), "who may sign up over HTTP: closed, invite or open")
	maxNotes := fs.Int("max-notes", envInt("POGO_MAX_NOTES"), "notes per user, 0 for unlimited")
	maxStorage := fs.String("max-storage", envOr("POGO_MAX_STORAGE", "0"), "note content per user (e.g. 50MB), 0 for unlimited")
	trustProxy := fs.Bool("trust-proxy", os.Getenv("POGO_TRUST_PROXY") == "1", "use X-Forwarded-For for client addresses")
	fs.Parse(args)

	switch *signup {
	case api.SignupClosed, api.SignupInvite, api.SignupOpen:
	default:
		return fmt.Errorf("--signup must be closed, invite or open, not %q", *signup)
	}
	maxBytes, err := parseSize(*maxStorage)
	if err != nil {
		return fmt.Errorf("--max-storage: %w", err)
	}
	if *maxNotes < 0 {
		return errors.New("--max-notes must be >= 0")
	}

	st, err := store.Open(*dbPath)
	if err != nil {
		return err
	}
	defer st.Close()
	st.SetLimits(store.Limits{MaxNotes: *maxNotes, MaxBytes: maxBytes})

	srv := &http.Server{
		Addr:              *addr,
		Handler:           api.New(st, api.Config{Version: version, Signup: *signup, TrustProxy: *trustProxy}).Handler(),
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       60 * time.Second,
		WriteTimeout:      60 * time.Second,
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		srv.Shutdown(shutdownCtx)
	}()

	log.Printf("pogo-pad %s listening on %s (db %s, signup %s)", version, *addr, *dbPath, *signup)
	if *cert != "" || *key != "" {
		err = srv.ListenAndServeTLS(*cert, *key)
	} else {
		err = srv.ListenAndServe()
	}
	if errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return err
}

const maxUserLen = 64

func user(args []string) error {
	if len(args) == 0 {
		return errors.New("user: expected create, list, rename or delete")
	}
	fs := flag.NewFlagSet("user "+args[0], flag.ExitOnError)
	dbPath := fs.String("db", defaultDB(), "SQLite database path")
	email := fs.String("email", "", "email address (create)")
	withPassword := fs.Bool("password", false, "set a password so the user can sign in from the apps (create)")
	clearPw := fs.Bool("clear", false, "remove the password (passwd)")
	fs.Parse(args[1:])

	wantArgs := map[string]int{"create": 1, "list": 0, "rename": 2, "email": 2, "passwd": 1, "delete": 1}
	n, ok := wantArgs[args[0]]
	if !ok {
		return fmt.Errorf("user: unknown subcommand %q", args[0])
	}
	if fs.NArg() != n {
		return fmt.Errorf("user %s: expected %d argument(s), got %d", args[0], n, fs.NArg())
	}

	st, err := store.Open(*dbPath)
	if err != nil {
		return err
	}
	defer st.Close()
	ctx := context.Background()

	switch args[0] {
	case "create":
		name := fs.Arg(0)
		if err := validUserName(name); err != nil {
			return err
		}
		var hash string
		if *withPassword {
			if hash, err = readPassword(); err != nil {
				return err
			}
		}
		u, err := st.CreateUser(ctx, name)
		if errors.Is(err, store.ErrExists) {
			return fmt.Errorf("user %q already exists", name)
		} else if err != nil {
			return err
		}
		if err := setupUser(ctx, st, u.ID, *email, hash); err != nil {
			st.DeleteUserByID(ctx, u.ID)
			return err
		}
		fmt.Printf("created user %q; now run: pogo-pad token create --user %s --name DEVICE\n", name, name)
	case "list":
		users, err := st.ListUsers(ctx)
		if err != nil {
			return err
		}
		tw := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
		fmt.Fprintln(tw, "ID\tNAME\tEMAIL\tPASSWORD\tCREATED\tTOKENS\tNOTES\tSIZE")
		for _, u := range users {
			email, pw := u.Email, "no"
			if email == "" {
				email = "-"
			}
			if u.HasPassword {
				pw = "yes"
			}
			fmt.Fprintf(tw, "%d\t%s\t%s\t%s\t%s\t%d\t%d\t%s\n", u.ID, u.Name, email, pw,
				u.CreatedAt.Format(time.DateTime), u.Tokens, u.Notes, formatSize(u.Bytes))
		}
		tw.Flush()
	case "rename":
		if err := validUserName(fs.Arg(1)); err != nil {
			return err
		}
		err := st.RenameUser(ctx, fs.Arg(0), fs.Arg(1))
		switch {
		case errors.Is(err, store.ErrNotFound):
			return fmt.Errorf("no user %q", fs.Arg(0))
		case errors.Is(err, store.ErrExists):
			return fmt.Errorf("user %q already exists", fs.Arg(1))
		case err != nil:
			return err
		}
		fmt.Printf("renamed %q to %q\n", fs.Arg(0), fs.Arg(1))
	case "email", "passwd":
		u, err := resolveUser(ctx, st, fs.Arg(0), false)
		if err != nil {
			return err
		}
		if args[0] == "email" {
			if fs.Arg(1) == "" {
				err = st.SetEmail(ctx, u.ID, "")
			} else {
				err = setupUser(ctx, st, u.ID, fs.Arg(1), "")
			}
			if err != nil {
				return err
			}
			fmt.Printf("updated the email of %q\n", u.Name)
			return nil
		}
		hash := ""
		if !*clearPw {
			if hash, err = readPassword(); err != nil {
				return err
			}
		}
		if err := st.SetPassword(ctx, u.ID, hash); err != nil {
			return err
		}
		if *clearPw {
			fmt.Printf("removed the password of %q; their existing tokens still work\n", u.Name)
		} else {
			fmt.Printf("set the password of %q\n", u.Name)
		}
	case "delete":
		err := st.DeleteUser(ctx, fs.Arg(0))
		if errors.Is(err, store.ErrNotFound) {
			return fmt.Errorf("no user %q", fs.Arg(0))
		}
		if err != nil {
			return err
		}
		fmt.Printf("deleted user %q with all their notes and tokens\n", fs.Arg(0))
	}
	return nil
}

func validUserName(name string) error {
	if name == "" || len(name) > maxUserLen || strings.ContainsFunc(name, unicode.IsSpace) {
		return fmt.Errorf("user name must be 1-%d characters without spaces", maxUserLen)
	}
	return nil
}

// setupUser sets the optional email and password hash of a user.
func setupUser(ctx context.Context, st *store.Store, uid int64, email, hash string) error {
	if email != "" {
		if _, err := mail.ParseAddress(email); err != nil || strings.ContainsAny(email, " <>") {
			return fmt.Errorf("invalid email address %q", email)
		}
		err := st.SetEmail(ctx, uid, email)
		if errors.Is(err, store.ErrEmailTaken) {
			return fmt.Errorf("%s is already used by another user", email)
		}
		if err != nil {
			return err
		}
	}
	if hash != "" {
		return st.SetPassword(ctx, uid, hash)
	}
	return nil
}

// readPassword reads a new password, from the terminal without echo (asked
// twice), or else from the first line of stdin, and returns its hash.
func readPassword() (string, error) {
	var pw string
	if fd := int(os.Stdin.Fd()); term.IsTerminal(fd) {
		fmt.Fprint(os.Stderr, "Password: ")
		p1, err := term.ReadPassword(fd)
		fmt.Fprintln(os.Stderr)
		if err != nil {
			return "", err
		}
		fmt.Fprint(os.Stderr, "Repeat password: ")
		p2, err := term.ReadPassword(fd)
		fmt.Fprintln(os.Stderr)
		if err != nil {
			return "", err
		}
		if string(p1) != string(p2) {
			return "", errors.New("passwords do not match")
		}
		pw = string(p1)
	} else {
		line, err := bufio.NewReader(os.Stdin).ReadString('\n')
		if err != nil && line == "" {
			return "", fmt.Errorf("reading password from stdin: %w", err)
		}
		pw = strings.TrimRight(line, "\r\n")
	}
	if err := password.Check(pw); err != nil {
		return "", err
	}
	return password.Hash(pw)
}

// resolveUser finds the user named name. With no name it falls back to the
// only user; if create is set and there are no users yet, it makes
// store.LegacyUser so single-user setups need no user commands.
func resolveUser(ctx context.Context, st *store.Store, name string, create bool) (store.User, error) {
	if name != "" {
		u, err := st.GetUser(ctx, name)
		if errors.Is(err, store.ErrNotFound) {
			return u, fmt.Errorf("no user %q (see pogo-pad user list)", name)
		}
		return u, err
	}
	users, err := st.ListUsers(ctx)
	if err != nil {
		return store.User{}, err
	}
	switch {
	case len(users) == 1:
		return users[0], nil
	case len(users) > 1:
		return store.User{}, errors.New("there are several users; pass --user")
	case !create:
		return store.User{}, errors.New("there are no users yet")
	}
	fmt.Fprintf(os.Stderr, "Created user %q.\n", store.LegacyUser)
	return st.CreateUser(ctx, store.LegacyUser)
}

func token(args []string) error {
	if len(args) == 0 {
		return errors.New("token: expected create, list or revoke")
	}
	fs := flag.NewFlagSet("token "+args[0], flag.ExitOnError)
	dbPath := fs.String("db", defaultDB(), "SQLite database path")
	name := fs.String("name", "", "device name (create)")
	userName := fs.String("user", "", "user the token belongs to (optional when there is only one user)")
	fs.Parse(args[1:])

	st, err := store.Open(*dbPath)
	if err != nil {
		return err
	}
	defer st.Close()
	ctx := context.Background()

	switch args[0] {
	case "create":
		if *name == "" {
			return errors.New("token create: --name is required")
		}
		u, err := resolveUser(ctx, st, *userName, true)
		if err != nil {
			return fmt.Errorf("token create: %w", err)
		}
		secret, err := st.CreateToken(ctx, u.ID, *name)
		if err != nil {
			return err
		}
		fmt.Fprintf(os.Stderr, "Token %q for user %q (shown only once — paste it into Pogo's or Pogo Pocket's Sync settings):\n", *name, u.Name)
		fmt.Println(secret)
	case "list":
		var uid int64
		if *userName != "" {
			u, err := resolveUser(ctx, st, *userName, false)
			if err != nil {
				return fmt.Errorf("token list: %w", err)
			}
			uid = u.ID
		}
		tokens, err := st.ListTokens(ctx, uid)
		if err != nil {
			return err
		}
		tw := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
		fmt.Fprintln(tw, "ID\tUSER\tNAME\tCREATED\tLAST USED\tSTATUS")
		for _, t := range tokens {
			used, status := "never", "active"
			if t.LastUsedAt != nil {
				used = t.LastUsedAt.Format(time.DateTime)
			}
			if t.Revoked {
				status = "revoked"
			}
			fmt.Fprintf(tw, "%d\t%s\t%s\t%s\t%s\t%s\n", t.ID, t.User, t.Name, t.CreatedAt.Format(time.DateTime), used, status)
		}
		tw.Flush()
	case "revoke":
		if fs.NArg() != 1 {
			return errors.New("token revoke: expected exactly one ID or NAME")
		}
		var uid int64
		if *userName != "" {
			u, err := resolveUser(ctx, st, *userName, false)
			if err != nil {
				return fmt.Errorf("token revoke: %w", err)
			}
			uid = u.ID
		}
		n, err := st.RevokeToken(ctx, uid, fs.Arg(0))
		switch {
		case errors.Is(err, store.ErrNotFound):
			return fmt.Errorf("no active token matches %q", fs.Arg(0))
		case errors.Is(err, store.ErrAmbiguous):
			return fmt.Errorf("%q %v; pass --user or use the token ID", fs.Arg(0), err)
		case err != nil:
			return err
		}
		fmt.Printf("revoked %d token(s)\n", n)
	default:
		return fmt.Errorf("token: unknown subcommand %q", args[0])
	}
	return nil
}

func invite(args []string) error {
	if len(args) == 0 {
		return errors.New("invite: expected create, list or delete")
	}
	fs := flag.NewFlagSet("invite "+args[0], flag.ExitOnError)
	dbPath := fs.String("db", defaultDB(), "SQLite database path")
	fs.Parse(args[1:])

	st, err := store.Open(*dbPath)
	if err != nil {
		return err
	}
	defer st.Close()
	ctx := context.Background()

	switch args[0] {
	case "create":
		code, err := st.CreateInvite(ctx)
		if err != nil {
			return err
		}
		fmt.Fprintln(os.Stderr, "Invite code (shown only once; it works for one signup when the server runs with --signup invite):")
		fmt.Println(code)
	case "list":
		invites, err := st.ListInvites(ctx)
		if err != nil {
			return err
		}
		tw := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
		fmt.Fprintln(tw, "ID\tCREATED\tSTATUS")
		for _, inv := range invites {
			status := "unused"
			if inv.UsedAt != nil {
				who := inv.UsedBy
				if who == "" {
					who = "a deleted user"
				}
				status = fmt.Sprintf("used by %s on %s", who, inv.UsedAt.Format(time.DateTime))
			}
			fmt.Fprintf(tw, "%d\t%s\t%s\n", inv.ID, inv.CreatedAt.Format(time.DateTime), status)
		}
		tw.Flush()
	case "delete":
		if fs.NArg() != 1 {
			return errors.New("invite delete: expected exactly one ID")
		}
		id, err := strconv.ParseInt(fs.Arg(0), 10, 64)
		if err != nil {
			return fmt.Errorf("invite delete: %q is not an ID", fs.Arg(0))
		}
		if err := st.DeleteInvite(ctx, id); errors.Is(err, store.ErrNotFound) {
			return fmt.Errorf("no unused invite with ID %d", id)
		} else if err != nil {
			return err
		}
		fmt.Printf("deleted invite %d\n", id)
	default:
		return fmt.Errorf("invite: unknown subcommand %q", args[0])
	}
	return nil
}

var sizeUnits = []struct {
	suffix string
	mult   int64
}{{"GB", 1 << 30}, {"MB", 1 << 20}, {"KB", 1 << 10}, {"B", 1}}

// parseSize parses a byte count such as 1048576, 512KB, 50MB or 2GB
// (binary multiples).
func parseSize(in string) (int64, error) {
	s := strings.ToUpper(strings.TrimSpace(in))
	mult := int64(1)
	for _, u := range sizeUnits {
		if n, ok := strings.CutSuffix(s, u.suffix); ok {
			s, mult = strings.TrimSpace(n), u.mult
			break
		}
	}
	n, err := strconv.ParseInt(s, 10, 64)
	if err != nil || n < 0 {
		return 0, fmt.Errorf("invalid size %q", in)
	}
	return n * mult, nil
}

func formatSize(n int64) string {
	for _, u := range sizeUnits {
		if n >= u.mult && u.mult > 1 {
			return fmt.Sprintf("%.1f %s", float64(n)/float64(u.mult), u.suffix)
		}
	}
	return fmt.Sprintf("%d B", n)
}

func envInt(key string) int {
	n, _ := strconv.Atoi(os.Getenv(key))
	return n
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}
