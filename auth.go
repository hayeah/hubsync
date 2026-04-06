package hubsync

import (
	"net/http"
	"strings"
)

// BearerToken is the shared secret for hub authentication.
type BearerToken string

// AuthMiddleware returns middleware that validates Bearer token auth.
// If token is empty, all requests are allowed (no auth).
func AuthMiddleware(token BearerToken, next http.HandlerFunc) http.HandlerFunc {
	if token == "" {
		return next
	}
	return func(w http.ResponseWriter, r *http.Request) {
		auth := r.Header.Get("Authorization")
		if !strings.HasPrefix(auth, "Bearer ") || auth[7:] != string(token) {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		next(w, r)
	}
}
