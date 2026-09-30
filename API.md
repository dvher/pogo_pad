# Sync API (v1)

The Pogo desktop and mobile apps copy these types. All bodies are JSON.

## Auth

Every endpoint except `/health` requires `Authorization: Bearer <token>`.
Tokens are created on the server with `pogo-pad token create [--user <user>] --name <device>`.
A missing or invalid token returns `401 {"error": "..."}`.

Each token belongs to one user, and every endpoint acts only on that user's data: notes, revs and
the E2E setup are all per user. Two users can use the same note id without conflict. Revs increase
per user, so a cursor is only meaningful for the user that received it.

## `GET /api/v1/health`

```json
{ "ok": true, "version": "dev" }
```

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

### What is synced

Synced: `id`, `content`, `color`, `deleted`.
Not synced (local to each device): window position and size, anchored, hidden.

Limits:
- 16 MiB per request
- 1 MiB of content per note
- ids up to 64 characters
