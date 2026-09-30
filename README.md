# 🟨 Pogo Pad

**The self-hosted landing pad for [Pogo](https://github.com/dvher/pogo) sticky notes, on the desktop and in [Pogo Pocket](https://github.com/dvher/pogo_pocket) on your phone.**

Your notes bounce between devices, and Pogo Pad is where they land. It's a single Go binary with
a SQLite database: no cgo, no external services, nothing to configure beyond a port. It runs on a
Raspberry Pi, a home server or a small VPS.

- **Multiple users:** one server can host notes for your whole household. Each user's notes and
  encryption settings are kept separate.
- **Per-device tokens:** create one for each laptop or phone, and revoke it if a device is lost.
- **Last write wins, per note:** simple and predictable conflict handling.
- **Optional end-to-end encryption:** Pogo can encrypt notes before upload, so Pogo Pad only ever
  stores unreadable data. The server never sees your passphrase or key.

## Run with Docker

```sh
docker compose up -d
docker compose exec pogo-pad pogo-pad token create --name laptop
```

The token is printed once. On an empty server this also creates a user called `default`. In Pogo, go to the tray icon → **Manage notes** → **Sync**. Enter the
server's address (e.g. `192.168.1.10`, port `8080`) and the token, then click **Test connection** and **Save**.

## Run from source

```sh
go build -o pogo-pad ./cmd/pogo-pad
./pogo-pad token create --name laptop   # prints the token once
./pogo-pad serve --addr :8080           # database: ./data/pogo-pad.db
```

## Multiple users

Every token belongs to one user, and a token only ever sees its own user's notes. To add someone:

```sh
pogo-pad user create alice
pogo-pad token create --user alice --name alice-phone
```

Once there is more than one user, `token create` needs `--user`. `pogo-pad user delete NAME` removes a
user with all their notes and tokens.

When you upgrade from a single-user version, the existing notes, tokens and E2E settings move to a
user called `default` (rename it with `pogo-pad user rename default NAME`). Devices keep syncing
without changes.

## Commands

| Command | What it does |
|---|---|
| `serve [--addr] [--db] [--tls-cert --tls-key]` | Run the API |
| `user create [--db] NAME` | Create a user |
| `user list [--db]` | List users with their token and note counts |
| `user rename [--db] OLD NEW` | Rename a user (their tokens keep working) |
| `user delete [--db] NAME` | Delete a user and all of their notes and tokens |
| `token create [--db] [--user USER] --name NAME` | Create a device token (shown once; only a hash is stored) |
| `token list [--db] [--user USER]` | List tokens and when each was last used |
| `token revoke [--db] [--user USER] ID\|NAME` | Revoke a token |
| `version` | Print the version |

Environment variables: `POGO_DB`, `POGO_ADDR`, `POGO_TLS_CERT`, `POGO_TLS_KEY`.

## HTTPS

Tokens are sent in a header, so use HTTPS for anything outside a trusted home network. You can either:
- pass `--tls-cert` and `--tls-key`, or
- put Pogo Pad behind a reverse proxy, for example Caddy:

  ```
  pogo.example.com {
      reverse_proxy localhost:8080
  }
  ```

## What the server stores

| Stored | Not stored |
|---|---|
| User names; per user: note ID, text, color, deleted flag, last-edit time, device ID | Window position, size, anchored or hidden state (these stay on each device) |
| With E2E on: only ciphertext, plus a salt and check value | Your E2E passphrase or key |
| SHA-256 hashes of tokens | Tokens themselves |

## Development

```sh
go test ./...
```

The wire protocol is documented in [API.md](API.md). Pogo and Pogo Pocket share one implementation of it (Pogo's `pkg/pogosync`).

## License

[MIT](LICENSE)
