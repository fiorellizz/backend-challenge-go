package postgres

import "time"

// Helpers between the domain's "zero value means absent" convention and
// SQL NULL. pgx encodes nil pointers as NULL and scans NULL into nil.

func nullStr(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

func nullInt(v int64) *int64 {
	if v == 0 {
		return nil
	}
	return &v
}

func nullTime(t time.Time) *time.Time {
	if t.IsZero() {
		return nil
	}
	u := t.UTC()
	return &u
}

func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

func derefTime(t *time.Time) time.Time {
	if t == nil {
		return time.Time{}
	}
	return t.UTC()
}
