package errs

import (
	"errors"
	"fmt"
	"testing"
)

func TestKindOf(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want Kind
	}{
		{"nil", nil, Internal},
		{"plain error", errors.New("boom"), Internal},
		{"kinded", E(Connect, errors.New("refused")), Connect},
		{"wrapped twice", fmt.Errorf("outer: %w", fmt.Errorf("inner: %w", E(Leak, errors.New("pii")))), Leak},
		{"first kind wins", E(Usage, E(Output, errors.New("x"))), Usage},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := KindOf(tt.err); got != tt.want {
				t.Errorf("KindOf() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestE(t *testing.T) {
	if E(Usage, nil) != nil {
		t.Error("E(kind, nil) should return nil")
	}
	base := errors.New("base")
	err := E(Output, base)
	if !errors.Is(err, base) {
		t.Error("E should keep the original error in the chain")
	}
	if err.Error() != "base" {
		t.Errorf("Error() = %q, want %q", err.Error(), "base")
	}
}

func TestKindString(t *testing.T) {
	tests := []struct {
		kind Kind
		want string
	}{
		{Internal, "ERROR"},
		{Usage, "USAGE"},
		{CheckFailed, "CHECK_FAILED"},
		{Connect, "CONNECT"},
		{Integrity, "INTEGRITY"},
		{Leak, "LEAK"},
		{Output, "OUTPUT"},
		{Incompatible, "INCOMPATIBLE"},
		{Kind(200), "ERROR"},
	}
	for _, tt := range tests {
		if got := tt.kind.String(); got != tt.want {
			t.Errorf("Kind(%d).String() = %q, want %q", tt.kind, got, tt.want)
		}
	}
}
