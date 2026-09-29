// Package lww implements last-write-wins conflict resolution for notes.
package lww

import "notes-server/internal/model"

// Wins reports whether incoming should replace existing.
// The newer UpdatedAt wins; ties are broken deterministically by DeviceID
// so every replica converges on the same value.
func Wins(incoming, existing model.Note) bool {
	if incoming.UpdatedAt != existing.UpdatedAt {
		return incoming.UpdatedAt > existing.UpdatedAt
	}
	return incoming.DeviceID > existing.DeviceID
}
