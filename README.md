# 🟨 Pogo Pad

**The self-hosted landing pad for [Pogo](https://github.com/dvher/pogo) sticky notes.**

Your notes bounce between devices, and Pogo Pad is where they land. It's a single Go binary with
a SQLite database: no cgo, no external services, nothing to configure beyond a port. It runs on a
Raspberry Pi, a home server or a small VPS.

- **Per-device tokens:** create one for each laptop or phone, and revoke it if a device is lost.
- **Last write wins, per note:** simple and predictable conflict handling.
- **Optional end-to-end encryption:** Pogo can encrypt notes before upload, so Pogo Pad only ever
  stores unreadable data. The server never sees your passphrase or key.

## Run with Docker

```sh
docker compose up -d
docker compose exec pogo-pad pogo-pad token create --name laptop
```

The token is printed once. In Pogo, go to the tray icon → **Manage notes** → **Sync**. Enter the
server's address (e.g. `192.168.1.10`, port `8080`) and the token, then click **Test connection** and **Save**.

## Run from source

```sh
go build -o pogo-pad ./cmd/pogo-pad
./pogo-pad token create --name laptop   # prints the token once
./pogo-pad serve --addr :8080           # database: ./data/pogo-pad.db
```

## Commands

| Command | What it does |
|---|---|
| `serve [--addr] [--db] [--tls-cert --tls-key]` | Run the API |
| `token create [--db] --name NAME` | Create a device token (shown once; only a hash is stored) |
| `token list [--db]` | List tokens and when each was last used |
| `token revoke [--db] ID\|NAME` | Revoke a token |
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
| Note ID, text, color, deleted flag, last-edit time, device ID | Window position, size, anchored or hidden state (these stay on each device) |
| With E2E on: only ciphertext, plus a salt and check value | Your E2E passphrase or key |
| SHA-256 hashes of tokens | Tokens themselves |

## Development

```sh
go test ./...
```

The wire protocol is documented in [API.md](API.md), and the Pogo apps implement exactly that.

## License

[MIT](LICENSE)
