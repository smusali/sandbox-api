package httpapi

import (
	"errors"
	"testing"
)

func TestTokenErrorIgnoresUnrelated(t *testing.T) {
	if TokenError(nil) != nil {
		t.Fatal("nil error mapped")
	}
	if TokenError(errors.New("other")) != nil {
		t.Fatal("unrelated error mapped")
	}
}
