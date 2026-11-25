package auth

import (
	"context"
	"testing"

	"github.com/google/uuid"
)

func TestPrincipalRoundTrip(t *testing.T) {
	userID := uuid.MustParse("11111111-1111-4111-8111-111111111111")
	principal := Principal{UserID: userID}

	ctx := WithPrincipal(context.Background(), principal)
	got, ok := PrincipalFrom(ctx)
	if !ok || got != principal {
		t.Fatalf("principal = %+v, ok=%v", got, ok)
	}

	child, cancel := context.WithCancel(ctx)
	defer cancel()
	got, ok = PrincipalFrom(child)
	if !ok || got != principal {
		t.Fatalf("child principal = %+v, ok=%v", got, ok)
	}
}

func TestPrincipalRejectsEmptyAndReplacement(t *testing.T) {
	if _, ok := PrincipalFrom(nil); ok {
		t.Fatal("nil context returned a principal")
	}
	if _, ok := PrincipalFrom(context.Background()); ok {
		t.Fatal("empty context returned a principal")
	}

	empty := WithPrincipal(nil, Principal{})
	if _, ok := PrincipalFrom(empty); ok {
		t.Fatal("empty user id was stored")
	}

	original := uuid.MustParse("11111111-1111-4111-8111-111111111111")
	other := uuid.MustParse("22222222-2222-4222-8222-222222222222")
	ctx := WithPrincipal(context.Background(), Principal{UserID: original})
	ctx = WithPrincipal(ctx, Principal{UserID: other})
	got, ok := PrincipalFrom(ctx)
	if !ok || got.UserID != original {
		t.Fatalf("principal = %+v, ok=%v", got, ok)
	}

	fromNil := WithPrincipal(nil, Principal{UserID: original})
	got, ok = PrincipalFrom(fromNil)
	if !ok || got.UserID != original {
		t.Fatalf("nil parent principal = %+v, ok=%v", got, ok)
	}
}
