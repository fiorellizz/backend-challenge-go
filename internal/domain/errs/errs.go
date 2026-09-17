// Package errs defines the error kinds shared by the whole domain. Every
// domain error wraps one of these sentinels so callers can classify failures
// with errors.Is without knowing the concrete cause.
package errs

import "errors"

var (
	// ErrValidation marks input that can never be accepted as sent. The caller
	// may correct the input and retry.
	ErrValidation = errors.New("validation error")

	// ErrConflict marks a request that contradicts already persisted state,
	// such as a reused idempotency key with a different payload.
	ErrConflict = errors.New("conflict")

	// ErrNotFound marks a lookup for an entity that does not exist.
	ErrNotFound = errors.New("not found")

	// ErrTransient marks an infrastructure failure that is expected to clear
	// on retry, such as a lost database connection.
	ErrTransient = errors.New("transient failure")
)
