package dialect

import (
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
)

// ErrUnknownScheme is returned by Lookup when no adapter handles a URL's scheme.
var ErrUnknownScheme = errors.New("dialect: unknown database URL scheme")

// registry maps URL schemes to adapters. The package uses one shared
// registry; tests make their own.
type registry struct {
	mu      sync.RWMutex
	schemes map[string]Dialect
}

func newRegistry() *registry { return &registry{schemes: make(map[string]Dialect)} }

var defaultRegistry = newRegistry()

// Register makes an adapter available under its URL schemes. It panics if a
// scheme is already taken or the adapter has no schemes, so that mistakes
// show up as soon as stuntdb starts.
func Register(d Dialect) { defaultRegistry.register(d) }

// Lookup returns the adapter for a database URL, chosen by the URL's scheme,
// such as "postgres://…" or "sqlite:./app.db". The error never contains the
// URL, which may hold a password.
func Lookup(url string) (Dialect, error) { return defaultRegistry.lookup(url) }

// Registered returns the names of the registered adapters, sorted.
func Registered() []string { return defaultRegistry.names() }

func (r *registry) register(d Dialect) {
	schemes := d.Schemes()
	if len(schemes) == 0 {
		panic("dialect: adapter " + d.Name() + " has no URL schemes")
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, s := range schemes {
		s = strings.ToLower(s)
		if other, taken := r.schemes[s]; taken {
			panic("dialect: URL scheme " + s + " is registered by both " + other.Name() + " and " + d.Name())
		}
		r.schemes[s] = d
	}
}

func (r *registry) lookup(url string) (Dialect, error) {
	scheme, _, found := strings.Cut(url, ":")
	if !found || !validScheme(scheme) {
		return nil, fmt.Errorf("%w: a database URL starts with a scheme such as sqlite: or postgres://", ErrUnknownScheme)
	}
	scheme = strings.ToLower(scheme)
	r.mu.RLock()
	d, ok := r.schemes[scheme]
	r.mu.RUnlock()
	if !ok {
		if len(scheme) == 1 {
			// "C:\data\app.db" looks like a scheme "c".
			return nil, fmt.Errorf("%w: write a file path as sqlite:%s:… rather than as a bare path", ErrUnknownScheme, scheme)
		}
		return nil, fmt.Errorf("%w %q", ErrUnknownScheme, scheme)
	}
	return d, nil
}

func (r *registry) names() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	var names []string
	for _, d := range r.schemes {
		if !slices.Contains(names, d.Name()) {
			names = append(names, d.Name())
		}
	}
	slices.Sort(names)
	return names
}

// validScheme follows RFC 3986: a letter, then letters, digits, "+", "-" or ".".
func validScheme(s string) bool {
	if s == "" {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z':
		case i > 0 && (c >= '0' && c <= '9' || c == '+' || c == '-' || c == '.'):
		default:
			return false
		}
	}
	return true
}
