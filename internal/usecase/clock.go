package usecase

import (
	"time"

	"github.com/google/uuid"
)

// Clock supplies the current time. Use cases never call time.Now directly
// so tests can pin timestamps.
type Clock func() time.Time

// SystemClock returns the wall clock in UTC at microsecond precision, the
// same precision PostgreSQL stores, so values round-trip unchanged.
func SystemClock() time.Time { return time.Now().UTC().Truncate(time.Microsecond) }

// newID generates a time-ordered UUID (v7), which keeps identifiers
// roughly sortable by creation in indexes and logs.
func newID() string { return uuid.Must(uuid.NewV7()).String() }
