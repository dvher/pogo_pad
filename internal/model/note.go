// Package model holds the wire types shared by the API and the store.
package model

// Note is a note as exchanged over the sync API.
// Only content-level fields are synced; window geometry, anchoring and
// visibility are local to each device.
type Note struct {
	ID        string `json:"id"`
	Content   string `json:"content"`
	Color     string `json:"color"`
	Deleted   bool   `json:"deleted"`
	UpdatedAt int64  `json:"updated_at"` // unix milliseconds, set by the editing client
	DeviceID  string `json:"device_id"`
	Rev       int64  `json:"rev,omitempty"` // server sequence number, ignored on input
}

// SyncRequest is the body of POST /api/v1/sync.
type SyncRequest struct {
	Cursor  int64  `json:"cursor"`
	Changes []Note `json:"changes"`
}

// SyncResponse is returned by POST /api/v1/sync.
type SyncResponse struct {
	Cursor  int64  `json:"cursor"`
	Changes []Note `json:"changes"`
	More    bool   `json:"more"` // true when more changes are pending; call again with Cursor
}

// E2EParams is the opaque end-to-end encryption setup shared by clients via
// /api/v1/e2e. Salt feeds the client's key derivation; Check is a known
// plaintext encrypted with the derived key so clients can verify a passphrase.
type E2EParams struct {
	KDF   string `json:"kdf"`
	Salt  string `json:"salt"`
	Check string `json:"check"`
}
