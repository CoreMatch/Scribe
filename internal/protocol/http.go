package protocol

import (
	"encoding/json"
	"net/http"
)

func WriteError(w http.ResponseWriter, status int, errorName, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(ErrorResponse{Error: errorName, ErrorMessage: message})
}

func WriteInvalidToken(w http.ResponseWriter) {
	WriteError(w, http.StatusForbidden, "ForbiddenOperationException", "Invalid token.")
}
