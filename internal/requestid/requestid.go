// Package requestid carries bounded request correlation IDs, never metric labels.
package requestid

import (
	"context"
	"crypto/rand"
	"encoding/hex"
)

const Header = "X-Request-ID"

type key struct{}

func Valid(id string) bool {
	if len(id) < 1 || len(id) > 64 {
		return false
	}
	for _, c := range id {
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '-' || c == '_' || c == '.') {
			return false
		}
	}
	return true
}
func With(ctx context.Context, id string) context.Context {
	if !Valid(id) {
		var b [16]byte
		if _, err := rand.Read(b[:]); err != nil {
			panic("request correlation entropy unavailable")
		}
		id = hex.EncodeToString(b[:])
	}
	return context.WithValue(ctx, key{}, id)
}
func From(ctx context.Context) string { id, _ := ctx.Value(key{}).(string); return id }
