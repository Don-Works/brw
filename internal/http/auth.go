package httpapi

import (
	"crypto/sha256"
	"crypto/subtle"
	"net/http"
	"strings"
)

// SetAuthToken makes every request carry "Authorization: Bearer <token>".
func (s *Server) SetAuthToken(token string) {
	if token == "" {
		s.authDigest = nil
		return
	}
	sum := sha256.Sum256([]byte(token))
	s.authDigest = sum[:]
}

// RequiresAuth reports whether a bearer token is configured.
func (s *Server) RequiresAuth() bool { return len(s.authDigest) > 0 }

func (s *Server) bearerGuard(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if len(s.authDigest) == 0 {
			next.ServeHTTP(w, r)
			return
		}
		values := r.Header.Values("Authorization")
		if len(values) == 1 && strings.HasPrefix(values[0], "Bearer ") {
			sum := sha256.Sum256([]byte(strings.TrimPrefix(values[0], "Bearer ")))
			if subtle.ConstantTimeCompare(sum[:], s.authDigest) == 1 {
				next.ServeHTTP(w, r)
				return
			}
		}
		w.Header().Set("WWW-Authenticate", `Bearer realm="brw"`)
		http.Error(w, "authentication required", http.StatusUnauthorized)
	})
}
