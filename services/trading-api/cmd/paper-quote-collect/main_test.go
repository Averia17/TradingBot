package main

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

func TestPublishTapeDoesNotReplaceOwnerTape(t *testing.T) {
	directory := t.TempDir()
	path := filepath.Join(directory, "tape.json")
	original := []byte("original tape")
	if err := publishTape(path, original); err != nil {
		t.Fatal(err)
	}
	if err := publishTape(path, []byte("different tape")); err == nil {
		t.Fatal("replaced a tape that an owner may already have pinned")
	}
	raw, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(raw, original) {
		t.Fatalf("owner tape = %q, read error = %v", raw, err)
	}
	entries, err := os.ReadDir(directory)
	if err != nil || len(entries) != 1 {
		t.Fatalf("published files = %v, read error = %v", entries, err)
	}
}
