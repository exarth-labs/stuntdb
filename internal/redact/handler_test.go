package redact

import (
	"bytes"
	"errors"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/exarth-labs/stuntdb/internal/value"
)

type stringer struct{ s string }

func (s stringer) String() string { return s.s }

func TestHandlerRedactsEverything(t *testing.T) {
	for _, format := range []string{"text", "json"} {
		t.Run(format, func(t *testing.T) {
			var buf bytes.Buffer
			var h slog.Handler = slog.NewTextHandler(&buf, nil)
			if format == "json" {
				h = slog.NewJSONHandler(&buf, nil)
			}
			r := New("canary-7f3a-masking-key")
			log := slog.New(r.Handler(h)).With("source", "postgres://admin:canary-7f3a@db/app")

			log.Info("duplicate entry 'canary-7f3a' in main.users",
				"error", errors.New("value 'canary-7f3a' too long"),
				"key", "canary-7f3a-masking-key",
				"row", value.String("canary-7f3a"), // value.Value shows its kind only
				"stringer", stringer{"token=canary-7f3a"},
				"any", []string{"'canary-7f3a'"},
				slog.Group("load", "table", "main.users", "detail", "'canary-7f3a'"),
				"rows", 42, "took", time.Second,
			)
			log.WithGroup("g").Warn("nested", "detail", "'canary-7f3a'")

			out := buf.String()
			if strings.Contains(out, "canary-7f3a") {
				t.Errorf("the log holds a canary:\n%s", out)
			}
			for _, kept := range []string{"main.users", "42", "load", "nested"} {
				if !strings.Contains(out, kept) {
					t.Errorf("the log lost %q:\n%s", kept, out)
				}
			}
		})
	}
}

func TestHandlerKeepsLevels(t *testing.T) {
	var buf bytes.Buffer
	h := New().Handler(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelWarn}))
	log := slog.New(h)
	log.Info("hidden")
	log.Warn("shown")
	if out := buf.String(); strings.Contains(out, "hidden") || !strings.Contains(out, "shown") {
		t.Errorf("levels were not respected:\n%s", out)
	}
}
