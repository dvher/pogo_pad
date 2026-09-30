# Sync API (v1)

The Pogo desktop and mobile apps copy these types. All bodies are JSON.

## Auth

Every endpoint except `/health`, `/signup` and `/login` requires `Authorization: Bearer <token>`.
A token is issued per device, either on the server with
`pogo-pad token create [--user <user>] --name <device>` or by [signing in](#accounts).
A missing or invalid token returns `401 {"error": "..."}`.

Each token belongs to one user, and every endpoint acts only on that user's data: notes, revs and
the E2E setup are all per user. Two users can use the same note id without conflict. Revs increase
per user, so a cursor is only meaningful for the user that received it.

## `GET /api/v1/health`

```json
{ "ok": true, "version": "dev", "signup": "closed" }
```

`signup` is `closed`, `invite` or `open` (see [`POST /api/v1/signup`](#post-apiv1signup)); apps can use
it to decide whether to offer a "Create account" option. Older servers leave it out, which means `closed`.

## `POST /api/v1/sync`

Request:

```json
{
  "cursor": 0,
  "changes": [
    { "id": "uuid", "content": "# markdown", "color": "yellow",
      "deleted": false, "updated_at": 1759170000000, "device_id": "uuid" }
  ]
}
```

- `cursor`: the `cursor` from the last successful response (0 for a first sync).
- `changes`: notes edited locally since the last sync (at most 5000 per request).
  - `updated_at`: unix **milliseconds** of the local edit.
  - `device_id`: a stable, random id for the client install.
  - To delete a note, send a tombstone: `deleted: true` (content may be empty).

Response:

```json
{ "cursor": 42, "changes": [ { "...note...": "", "rev": 42 } ], "more": false }
```

- `changes`: every note whose server revision is newer than the request `cursor`, up to 500 of them. This includes changes the same request just applied.
- `more`: true when more changes are waiting. Call again with the new `cursor` (and no `changes`) until it is false.

### Conflict resolution

Last write wins, per note:
1. The change with the larger `updated_at` is kept.
2. On a tie, the larger `device_id` (string comparison) is kept.

Changes that lose are dropped silently. Clients should apply incoming notes with the same rule, so they keep a local edit made after the request was sent.

### End-to-end encryption (optional)

Clients may encrypt notes before upload. The server stores only ciphertext and never sees the key.

When E2E is on, a note is sent like this:
- `content = "e2e:v1:" + base64(nonce ‖ AES-256-GCM(json{"content","color"}))`, using standard base64 without padding and a 12-byte nonce
- `color = ""`

The key is `Argon2id(passphrase, salt, t=3, m=64MiB, p=4, 32 bytes)`.

The server keeps the shared, non-secret setup as an opaque blob:

| Request | Result |
|---|---|
| `GET /api/v1/e2e` | `200 {"kdf":"argon2id","salt":"<b64>","check":"<b64>"}` (unpadded base64) or `404` if not set up |
| `PUT /api/v1/e2e` | stores the blob; `409` if already set (append `?force=1` to replace, e.g. after a passphrase change) |
| `DELETE /api/v1/e2e` | `204`; turns E2E off (clients then re-upload plaintext) |

`check` is the string `notes-e2e-check`, encrypted the same way as a note. Clients decrypt it to confirm a passphrase is right.

### Quotas

A server may limit how many notes, and how many bytes of `content`, each user keeps (deleted notes
don't count). A sync whose changes would take the user over a limit is rejected as a whole with
`507 {"error": "storage quota exceeded; ..."}`. Nothing is applied. A sync that only shrinks usage
always succeeds, so a user over the limit can still delete notes. `GET /api/v1/account` shows the
limits and current usage.

### What is synced

Synced: `id`, `content`, `color`, `deleted`.
Not synced (local to each device): window position and size, anchored, hidden.

Limits:
- 16 MiB per request
- 1 MiB of content per note
- ids up to 64 characters

## Accounts

Users who have a password can sign in from an app instead of pasting a token. Every sign-in creates
a new device token. Endpoints that check a password are rate-limited per client address: 10
attempts, refilling over a minute. Over the limit they return `429` with `Retry-After`.

Names chosen at signup are 3-32 characters: lowercase letters, digits, `.`, `_` or `-`. Passwords
are 8-256 characters. Emails are optional, unique and compared case-insensitively.

### `POST /api/v1/signup`

```json
{ "name": "alice", "email": "alice@example.com", "password": "…", "device": "Alice's laptop", "invite": "inv_…" }
```

`email` is optional. `invite` is required only when the server's signup mode is `invite`.
Returns `201 {"token": "pogo_…", "user": {"name": "alice", "email": "alice@example.com"}}`.
Errors: `403` if signup is closed or the invite is invalid or used, `409` if the name or email is
taken, `400` for invalid fields.

### `POST /api/v1/login`

```json
{ "login": "alice", "password": "…", "device": "Alice's phone" }
```

`login` is the user name, or the email if it contains `@`. Returns `200` with the same body as
signup, or `401 {"error": "wrong user name or password"}`.

### `POST /api/v1/logout`

Revokes the token the request was made with. Returns `204`.

### `GET /api/v1/account`

```json
{
  "name": "alice", "email": "alice@example.com", "has_password": true, "created_at": 1790000000,
  "usage": { "notes": 12, "bytes": 3400 },
  "limits": { "max_notes": 0, "max_bytes": 52428800 }
}
```

A limit of `0` means unlimited. `created_at` is in unix seconds.

### Changing the account

These need the current password, so a stolen device token alone can't take over or delete the
account. A wrong password returns `403`. So does an account without a password (made with the CLI
and never given one).

| Request | Body | Result |
|---|---|---|
| `PUT /api/v1/account/password` | `{"current", "new", "revoke_other_devices": bool}` | `204` |
| `PUT /api/v1/account/email` | `{"email", "password"}`; an empty `email` removes it | `204`, or `409` if taken |
| `DELETE /api/v1/account` | `{"password"}` | `204`; the user's notes, E2E setup and tokens are deleted |

### Devices

| Request | Result |
|---|---|
| `GET /api/v1/devices` | `200 [{"id", "name", "created_at", "last_used_at", "current"}]`. `current` marks the calling token, and `last_used_at` is `null` if the token was never used |
| `DELETE /api/v1/devices/{id}` | `204`; revokes one of the user's own tokens, or returns `404` |
