package mask

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"testing"

	"golang.org/x/crypto/bcrypt"
)

// wordListHashes pins every word list of generator version 1: the SHA-256
// of its entries, joined by newlines. Comments may change freely, but a
// changed entry changes masked values: give it a new GeneratorVersion, then
// update this table.
var wordListHashes = map[string]string{
	"area_codes":       "6db17682d0a55a2e96943f791ad51ba3ea99d1d5351456d78b74f83e4c611057",
	"cities":           "02c0f770a93b8037716124bbaad58d22f2a1ec82a33cfd8e4b09bbc3162c4d21",
	"company_suffixes": "67e15731e5008d970baf468593c6e9e43b121017c4fac57ca29f39a78a77d6c2",
	"first_names":      "aadbd263d35c57eda9ab7dd5c1bd022a7e6f201517f96a4e999c80b8b58c329d",
	"last_names":       "40f95a80f4c57b058c00f71db234a4e2049a8565f898f768437047dd4cb1f982",
	"lorem":            "06bff231397fd4bbabaa07e31df958c0261c3a30fbf77d11ed4096afc79f0a4c",
	"states":           "cfb190d4f14c2474f7220a75e11d32078b1ed64b6d3a5efafc36365ecaafcbc1",
	"street_suffixes":  "2428f064e315905e4d1ebdd20032bde0a2c27260b9db200dc46e44f59eeed85d",
	"streets":          "9476c601d4801189fa1ce0e1c930bf5d221181f1a0a7578fb75af2a1c278a60a",
}

func TestWordListsArePinned(t *testing.T) {
	entries, err := wordFiles.ReadDir("words")
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != len(wordListHashes) {
		t.Errorf("%d word lists, %d pinned", len(entries), len(wordListHashes))
	}
	for name, want := range wordListHashes {
		sum := sha256.Sum256([]byte(strings.Join(loadWords(name), "\n")))
		if got := hex.EncodeToString(sum[:]); got != want {
			t.Errorf("word list %s changed (sha256 %s); masked values change with it, so it needs a new GeneratorVersion", name, got)
		}
	}
}

func TestLoadWordsRefuses(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Error("a missing word list loads")
		}
	}()
	loadWords("missing")
}

func TestFitting(t *testing.T) {
	list := []string{"ab", "abcd", "a"}
	if got := fitting(list, 2); strings.Join(got, ",") != "ab,a" {
		t.Errorf("fitting = %v", got)
	}
	if got := fitting(list, 0); len(got) != 3 {
		t.Errorf("fitting without a limit = %v", got)
	}
	if shortest(list) != 1 {
		t.Error("shortest")
	}
}

// TestDefaultPasswordHash checks, with Go's bcrypt, that the default hash
// is the hash of the documented password.
func TestDefaultPasswordHash(t *testing.T) {
	if err := bcrypt.CompareHashAndPassword([]byte(DefaultPasswordHash), []byte("stuntdb")); err != nil {
		t.Errorf("DefaultPasswordHash is not the bcrypt hash of stuntdb: %v", err)
	}
	if cost, _ := bcrypt.Cost([]byte(DefaultPasswordHash)); cost != 10 {
		t.Errorf("cost %d", cost)
	}
}
