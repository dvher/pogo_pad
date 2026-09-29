# Sync API (v1)

The desktop and mobile clients copy these types. All bodies are JSON.

## Auth

Every endpoint except `/health` requires `Authorization: Bearer <token>`.
Tokens are created on the server with `notes-server token create --name <device>`.
A missing or invalid token returns `401 {"error": "..."}`.

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

### What is synced

Synced: `id`, `content`, `color`, `deleted`.
Not synced (local to each device): window position and size, anchored, hidden.

Limits:
- 16 MiB per request
- 1 MiB of content per note
- ids up to 64 characters
