// Command pogo-pad is the self-hosted sync server for Pogo sticky notes.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"text/tabwriter"
	"time"
	"unicode"

	"github.com/dvher/pogo_pad/internal/api"
	"github.com/dvher/pogo_pad/internal/store"
)

var version = "dev"

const usage = `pogo-pad — self-hosted sync server for Pogo

Usage:
  pogo-pad serve [--addr :8080] [--db PATH] [--tls-cert FILE --tls-key FILE]
  pogo-pad user create [--db PATH] NAME
  pogo-pad user list [--db PATH]
  pogo-pad user rename [--db PATH] OLD NEW
  pogo-pad user delete [--db PATH] NAME
  pogo-pad token create [--db PATH] [--user USER] --name NAME
  pogo-pad token list [--db PATH] [--user USER]
  pogo-pad token revoke [--db PATH] [--user USER] ID|NAME
  pogo-pad version

Each user has their own notes and end-to-end encryption settings; a token
only reaches its user's data. --user may be left out while there is only one
user (on an empty database, token create makes a user called "default").

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
	fs.Parse(args)

	st, err := store.Open(*dbPath)
	if err != nil {
		return err
	}
	defer st.Close()

	srv := &http.Server{
		Addr:              *addr,
		Handler:           api.New(st, version).Handler(),
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

	log.Printf("pogo-pad %s listening on %s (db %s)", version, *addr, *dbPath)
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
	fs.Parse(args[1:])

	wantArgs := map[string]int{"create": 1, "list": 0, "rename": 2, "delete": 1}
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
		if _, err := st.CreateUser(ctx, name); errors.Is(err, store.ErrExists) {
			return fmt.Errorf("user %q already exists", name)
		} else if err != nil {
			return err
		}
		fmt.Printf("created user %q; now run: pogo-pad token create --user %s --name DEVICE\n", name, name)
	case "list":
		users, err := st.ListUsers(ctx)
		if err != nil {
			return err
		}
		tw := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
		fmt.Fprintln(tw, "ID\tNAME\tCREATED\tTOKENS\tNOTES")
		for _, u := range users {
			fmt.Fprintf(tw, "%d\t%s\t%s\t%d\t%d\n", u.ID, u.Name, u.CreatedAt.Format(time.DateTime), u.Tokens, u.Notes)
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

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}
