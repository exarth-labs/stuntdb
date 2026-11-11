package mask

import (
	"crypto/aes"
	"crypto/hkdf"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
)

// GeneratorVersion is the version of the fake-value generators: the seed
// derivation, the random stream, the word lists and every transformer. The
// lockfile records it, so that an upgrade of stuntdb never changes masked
// values without a reviewed lockfile change. Any change to what a
// transformer outputs for a given key and input needs a new version.
const GeneratorVersion = 1

// MinKeyLength is the shortest masking key accepted, in bytes.
const MinKeyLength = 32

// The HKDF info strings that separate the subkeys. They are part of
// generator version 1: changing one changes every masked value.
const (
	infoSeed        = "stuntdb/mask/v1/seed"
	infoFF1         = "stuntdb/mask/v1/ff1"
	infoFingerprint = "stuntdb/mask/v1/fingerprint"
)

// Keys holds the subkeys derived from a masking key. It never keeps the
// masking key itself, and never prints a key: every fmt verb and log/slog
// show only its fingerprint, for a Keys and a *Keys alike. It is safe for
// concurrent use.
type Keys struct {
	seed        []byte // HMAC-SHA256 key for seeds
	ff1         *FF1   // keyed with an AES-256 key
	fingerprint string
}

// NewKeys derives the subkeys from a masking key with HKDF-SHA256 (RFC 5869).
// Each purpose gets its own subkey, so that the subkeys are independent: the
// fingerprint, which is published in manifests, reveals nothing about the
// keys that mask values.
func NewKeys(masterKey []byte) (*Keys, error) {
	if len(masterKey) < MinKeyLength {
		return nil, fmt.Errorf("mask: the masking key must have at least %d bytes", MinKeyLength)
	}
	derive := func(info string, n int) []byte {
		// HKDF-Expand only fails for an output longer than 255 hash blocks.
		k, err := hkdf.Key(sha256.New, masterKey, nil, info, n)
		if err != nil {
			panic("mask: HKDF: " + err.Error())
		}
		return k
	}
	block, err := aes.NewCipher(derive(infoFF1, 32))
	if err != nil {
		return nil, fmt.Errorf("mask: %w", err)
	}
	return &Keys{
		seed:        derive(infoSeed, sha256.Size),
		ff1:         NewFF1(block),
		fingerprint: "hkdf-sha256:" + hex.EncodeToString(derive(infoFingerprint, 16)),
	}, nil
}

// The methods of Keys have value receivers, so that they belong to both
// Keys and *Keys: a method on *Keys only would not stop fmt from printing
// the fields of a Keys value.

// Fingerprint identifies the masking key without revealing it, such as
// "hkdf-sha256:3f2a…". Two outputs with the same fingerprint were masked
// with the same key. It goes in the manifest of every output.
func (k Keys) Fingerprint() string { return k.fingerprint }

// String describes the keys by their fingerprint only.
func (k Keys) String() string { return "mask.Keys(" + k.fingerprint + ")" }

// Format makes every fmt verb (%v, %+v, %#v, %x, …) print only what String
// prints. Without it, %#v would print the subkeys.
func (k Keys) Format(f fmt.State, _ rune) { _, _ = io.WriteString(f, k.String()) }

// LogValue makes log/slog print only what String prints.
func (k Keys) LogValue() slog.Value { return slog.StringValue(k.String()) }

// errNoKeys is returned when a keyed transformer runs without keys.
var errNoKeys = errors.New("mask: no masking keys")
