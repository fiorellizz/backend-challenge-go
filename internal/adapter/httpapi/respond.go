package httpapi

import (
	"encoding/json"
	"net/http"
)

// writeJSON encodes v with the given status. Encoding failures are
// programming errors (the payloads are our own structs), so they surface
// as a 500 with no body rather than a half-written response.
func writeJSON(w http.ResponseWriter, status int, v any) {
	body, err := json.Marshal(v)
	if err != nil {
		http.Error(w, "", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write(body)
}
