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
	"syscall"
	"text/tabwriter"
	"time"

	"github.com/dvher/pogo_pad/internal/api"
	"github.com/dvher/pogo_pad/internal/store"
)

var version = "dev"

const usage = `pogo-pad — self-hosted sync server for Pogo

Usage:
  pogo-pad serve [--addr :8080] [--db PATH] [--tls-cert FILE --tls-key FILE]
  pogo-pad token create [--db PATH] --name NAME
  pogo-pad token list [--db PATH]
  pogo-pad token revoke [--db PATH] ID|NAME
  pogo-pad version

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

func token(args []string) error {
	if len(args) == 0 {
		return errors.New("token: expected create, list or revoke")
	}
	fs := flag.NewFlagSet("token "+args[0], flag.ExitOnError)
	dbPath := fs.String("db", defaultDB(), "SQLite database path")
	name := fs.String("name", "", "device name (create)")
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
		secret, err := st.CreateToken(ctx, *name)
		if err != nil {
			return err
		}
		fmt.Fprintf(os.Stderr, "Token for %q (shown only once — paste it into Pogo's Sync settings):\n", *name)
		fmt.Println(secret)
	case "list":
		tokens, err := st.ListTokens(ctx)
		if err != nil {
			return err
		}
		tw := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
		fmt.Fprintln(tw, "ID\tNAME\tCREATED\tLAST USED\tSTATUS")
		for _, t := range tokens {
			used, status := "never", "active"
			if t.LastUsedAt != nil {
				used = t.LastUsedAt.Format(time.DateTime)
			}
			if t.Revoked {
				status = "revoked"
			}
			fmt.Fprintf(tw, "%d\t%s\t%s\t%s\t%s\n", t.ID, t.Name, t.CreatedAt.Format(time.DateTime), used, status)
		}
		tw.Flush()
	case "revoke":
		if fs.NArg() != 1 {
			return errors.New("token revoke: expected exactly one ID or NAME")
		}
		n, err := st.RevokeToken(ctx, fs.Arg(0))
		if errors.Is(err, store.ErrNotFound) {
			return fmt.Errorf("no active token matches %q", fs.Arg(0))
		}
		if err != nil {
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
