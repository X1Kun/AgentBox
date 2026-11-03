package httpauth

import (
	"net/http/httptest"
	"strings"
	"testing"
)

func TestConfiguredPrincipalsAndUnambiguousBearerHeader(t *testing.T) {
	principals := []Principal{{"alice", strings.Repeat("a", 32)}, {"bob", strings.Repeat("b", 32)}}
	a, err := New(principals)
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range principals {
		r := httptest.NewRequest("GET", "/", nil)
		r.Header.Set("Authorization", "Bearer "+p.Token)
		if got := a.Authenticate(r); got != p.ID {
			t.Fatal(got)
		}
		r.Header.Add("Authorization", "Bearer "+p.Token)
		if a.Authenticate(r) != "" {
			t.Fatal("accepted duplicate header")
		}
	}
	for _, p := range [][]Principal{nil, {{"bad user", strings.Repeat("a", 32)}}, {{"alice", "short"}}, {{"alice", strings.Repeat("a", 32)}, {"bob", strings.Repeat("a", 32)}}} {
		if _, err = New(p); err == nil {
			t.Fatal("accepted invalid credentials")
		}
	}
}
