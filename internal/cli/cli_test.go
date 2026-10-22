package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/exarth-labs/stuntdb/internal/buildinfo"
	"github.com/exarth-labs/stuntdb/internal/errs"
	"github.com/exarth-labs/stuntdb/internal/redact"
)

func TestExecute(t *testing.T) {
	tests := []struct {
		name       string
		args       []string
		wantKind   errs.Kind // checked only when wantErr is true
		wantErr    bool
		wantStdout string // substring
		wantStderr string // substring; empty means stderr must be empty
	}{
		{name: "no arguments shows help", args: nil, wantStdout: "Usage:"},
		{name: "version", args: []string{"version"}, wantStdout: "stuntdb "},
		{name: "version flag", args: []string{"--version"}, wantStdout: "stuntdb "},
		{name: "unknown command", args: []string{"nope"}, wantErr: true, wantKind: errs.Usage, wantStderr: "unknown command"},
		{name: "unknown flag", args: []string{"version", "--nope"}, wantErr: true, wantKind: errs.Usage, wantStderr: "unknown flag"},
		{name: "unexpected argument", args: []string{"version", "extra"}, wantErr: true, wantKind: errs.Usage, wantStderr: "Run 'stuntdb --help'"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			err := Execute(context.Background(), tt.args, &stdout, &stderr)

			if tt.wantErr {
				if err == nil {
					t.Fatal("expected an error, got nil")
				}
				if got := errs.KindOf(err); got != tt.wantKind {
					t.Errorf("kind = %v, want %v", got, tt.wantKind)
				}
			} else if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if !strings.Contains(stdout.String(), tt.wantStdout) {
				t.Errorf("stdout = %q, want it to contain %q", stdout.String(), tt.wantStdout)
			}
			if tt.wantStderr == "" && stderr.Len() > 0 {
				t.Errorf("stderr should be empty, got %q", stderr.String())
			}
			if !strings.Contains(stderr.String(), tt.wantStderr) {
				t.Errorf("stderr = %q, want it to contain %q", stderr.String(), tt.wantStderr)
			}
		})
	}
}

// Errors are redacted on their way to the screen, keep their kind, and the
// usage hint (our own text) is printed as it is.
func TestReportErrorRedacts(t *testing.T) {
	const canary = "canary-7f3a"
	redactor := redact.New()
	redactor.AddURL("postgres://admin:" + canary + "@db.internal/app")
	tests := []struct {
		name string
		err  error
		kind errs.Kind
	}{
		{"a row value", errs.E(errs.Integrity, errors.New("Duplicate entry '"+canary+"' for key 'email'")), errs.Integrity},
		{"a connection URL", errs.E(errs.Connect, errors.New("cannot reach postgres://admin:"+canary+"@db.internal/app")), errs.Connect},
		{"a password on its own", errs.E(errs.Connect, errors.New("password "+canary+" rejected")), errs.Connect},
		{"an error without a kind", errors.New("bad flag value '" + canary + "'"), errs.Usage},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var stderr bytes.Buffer
			err := reportError(&stderr, redactor, tt.err)
			if strings.Contains(stderr.String(), canary) || strings.Contains(err.Error(), canary) {
				t.Errorf("the canary came through: %q", stderr.String())
			}
			if errs.KindOf(err) != tt.kind {
				t.Errorf("kind = %v, want %v", errs.KindOf(err), tt.kind)
			}
		})
	}

	var stderr bytes.Buffer
	_ = reportError(&stderr, redactor, errs.E(errs.Usage, errors.New("bad")))
	if !strings.Contains(stderr.String(), "Run 'stuntdb --help' for usage.") {
		t.Errorf("the usage hint was changed: %q", stderr.String())
	}
}

func TestVersionJSON(t *testing.T) {
	var stdout, stderr bytes.Buffer
	if err := Execute(context.Background(), []string{"version", "--json"}, &stdout, &stderr); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	var got buildinfo.Info
	if err := json.Unmarshal(stdout.Bytes(), &got); err != nil {
		t.Fatalf("output is not valid JSON: %v\n%s", err, stdout.String())
	}
	if got != buildinfo.Get() {
		t.Errorf("JSON = %+v, want %+v", got, buildinfo.Get())
	}
	if stderr.Len() > 0 {
		t.Errorf("stderr should be empty, got %q", stderr.String())
	}
}

func TestWriteVersionText(t *testing.T) {
	info := buildinfo.Info{Version: "v0.0.1", Commit: "abc123", Date: "2026-10-20T00:00:00Z", Go: "go1.27.0", Platform: "linux/amd64"}
	var buf bytes.Buffer
	if err := writeVersion(&buf, info, false); err != nil {
		t.Fatal(err)
	}
	want := "stuntdb v0.0.1\n  commit:   abc123\n  built:    2026-10-20T00:00:00Z\n  go:       go1.27.0\n  platform: linux/amd64\n"
	if buf.String() != want {
		t.Errorf("got:\n%s\nwant:\n%s", buf.String(), want)
	}
}
