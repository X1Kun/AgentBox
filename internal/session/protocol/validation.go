package protocol

import (
	"path"
	"regexp"
	"strings"
	"unicode/utf8"
)

var identifier = regexp.MustCompile(`^[A-Za-z0-9._-]{1,160}$`)

func ValidID(id string) bool { return id != "." && id != ".." && identifier.MatchString(id) }

var requestIdentifier = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:-]{0,127}$`)

func ValidRequestID(id string) bool { return requestIdentifier.MatchString(id) }
func ValidPath(p string) bool {
	if p == "" || len(p) > 512 || !utf8.ValidString(p) || strings.HasPrefix(p, "/") || strings.ContainsAny(p, "\\\x00") || path.Clean(p) != p {
		return false
	}
	for _, part := range strings.Split(p, "/") {
		if part == "." || part == ".." || part == "" {
			return false
		}
	}
	return true
}
