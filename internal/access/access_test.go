package access

import (
	"context"
	"fmt"
	"go.xchunk.org/vela/api"
	"testing"
)

func TestScopesAndPrefixBoundaries(t *testing.T) {
	i := Identity{Scope: api.ScopeRW, Prefix: "team/"}
	if Require(i, api.ScopeRO, "team/file") != nil || Require(i, api.ScopeRW, "team/file") != nil {
		t.Fatal("valid access rejected")
	}
	if Require(i, api.ScopeAdmin) == nil || Require(i, api.ScopeRO, "team-other/file") == nil {
		t.Fatal("authority escaped")
	}
	for _, p := range []string{"", "te", "team/"} {
		out, err := ListingPrefix(i, p)
		if err != nil || out != "team/" {
			t.Fatalf("ancestor should narrow")
		}
	}
	if _, err := ListingPrefix(i, "other/"); err == nil {
		t.Fatal("unrelated listing accepted")
	}
	c := NewCredential("dummy-secret")
	if fmt.Sprintf("%v %#v", c, c) != "[redacted] [redacted]" {
		t.Fatal("credential printed")
	}
}

func TestRequestContextCannotFallBackToStaticCredentials(t *testing.T) {
	r := ContextResolver{}
	if _, err := r.Resolve(context.Background()); err == nil {
		t.Fatal("unauthenticated context accepted")
	}
	i := Identity{Subject: "grant-a", Scope: api.ScopeRO, Credential: NewCredential("dummy")}
	resolved, err := r.Resolve(WithIdentity(context.Background(), i))
	if err != nil || resolved.Subject != i.Subject {
		t.Fatal("verified context lost identity")
	}
}
