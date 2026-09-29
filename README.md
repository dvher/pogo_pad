# notes-server

A self-hosted sync backend for the sticky-notes apps. It is a single Go binary with a SQLite database and no cgo.

## Run with Docker

```sh
docker compose up -d
docker compose exec server notes-server token create --name laptop
```

Paste the printed token and the server address (e.g. `http://192.168.1.10:8080`) into **Manage notes → Sync** in the desktop app. Create one token per device.

## Run from source

```sh
go build -o notes-server ./cmd/notes-server
./notes-server token create --name laptop    # prints the token once
./notes-server serve --addr :8080            # db defaults to ./data/notes.db
```

## Commands

| Command | Description |
|---|---|
| `serve [--addr] [--db] [--tls-cert --tls-key]` | Run the API |
| `token create [--db] --name NAME` | Create a token (printed once; only a hash is stored) |
| `token list [--db]` | List tokens and when they were last used |
| `token revoke [--db] ID\|NAME` | Revoke a token |

Environment variables: `NOTES_DB`, `NOTES_ADDR`, `NOTES_TLS_CERT`, `NOTES_TLS_KEY`.

## TLS

Tokens travel in a header, so use HTTPS outside a trusted LAN. You can either:
- pass `--tls-cert/--tls-key`, or
- put the server behind a reverse proxy such as Caddy (`notes.example.com { reverse_proxy localhost:8080 }`).

## Development

```sh
go test ./...
```

See [API.md](API.md) for the wire protocol.
