package main

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/exarth-labs/stuntdb/internal/errs"
)

func TestExitCode(t *testing.T) {
	canceled, cancel := context.WithCancel(context.Background())
	cancel()

	tests := []struct {
		name string
		ctx  context.Context
		err  error
		want int
	}{
		{"success", context.Background(), nil, 0},
		{"error without a kind is internal", context.Background(), errors.New("boom"), 1},
		{"usage", context.Background(), errs.E(errs.Usage, errors.New("bad flag")), 2},
		{"check failed", context.Background(), errs.E(errs.CheckFailed, errors.New("drift")), 3},
		{"connect", context.Background(), errs.E(errs.Connect, errors.New("refused")), 4},
		{"integrity", context.Background(), errs.E(errs.Integrity, errors.New("broken fk")), 5},
		{"leak", context.Background(), errs.E(errs.Leak, errors.New("pii found")), 6},
		{"output", context.Background(), errs.E(errs.Output, errors.New("disk full")), 7},
		{"incompatible", context.Background(), errs.E(errs.Incompatible, errors.New("lossy")), 8},
		{"kind survives wrapping", context.Background(), fmt.Errorf("run: %w", errs.E(errs.Leak, errors.New("pii"))), 6},
		{"interrupted", canceled, fmt.Errorf("copy: %w", context.Canceled), 130},
		{"canceled error without a signal is internal", context.Background(), context.Canceled, 1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := exitCode(tt.ctx, tt.err); got != tt.want {
				t.Errorf("exitCode() = %d, want %d", got, tt.want)
			}
		})
	}
}
