// Package httpauth verifies configured API principals without logging credentials.
package httpauth

import (
	"crypto/sha256"
	"crypto/subtle"
	"errors"
	"net/http"
	"regexp"
	"strings"
)

type Principal struct {
	ID    string `json:"id"`
	Token string `json:"token"`
}
type credential struct {
	principal string
	sum       [32]byte
}
type Authenticator struct{ credentials []credential }

var validPrincipal = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_-]{0,62}$`)

func New(principals []Principal) (*Authenticator, error) {
	if len(principals) == 0 || len(principals) > 64 {
		return nil, errors.New("invalid configured principal count")
	}
	a := &Authenticator{}
	seen := map[[32]byte]bool{}
	for _, p := range principals {
		if !validPrincipal.MatchString(p.ID) || len(p.Token) < 32 || len(p.Token) > 4096 || strings.IndexFunc(p.Token, func(r rune) bool { return r < 33 || r > 126 }) >= 0 {
			return nil, errors.New("invalid configured principal credentials")
		}
		sum := sha256.Sum256([]byte(p.Token))
		if seen[sum] {
			return nil, errors.New("duplicate configured token")
		}
		seen[sum] = true
		a.credentials = append(a.credentials, credential{p.ID, sum})
	}
	return a, nil
}
func (a *Authenticator) Authenticate(r *http.Request) string {
	values := r.Header.Values("Authorization")
	if len(values) != 1 || !strings.HasPrefix(values[0], "Bearer ") || len(values[0]) > 4103 {
		return ""
	}
	sum := sha256.Sum256([]byte(strings.TrimPrefix(values[0], "Bearer ")))
	principal := ""
	for _, c := range a.credentials {
		if subtle.ConstantTimeCompare(sum[:], c.sum[:]) == 1 {
			principal = c.principal
		}
	}
	return principal
}
