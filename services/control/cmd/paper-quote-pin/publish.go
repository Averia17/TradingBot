package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"unicode/utf8"

	"tradingbot/control/internal/contract"
	"tradingbot/control/internal/ledger"
	"tradingbot/control/internal/papersim"
)

type quotePin struct {
	Version string   `json:"paper_quote_pin_schema_version"`
	JobID   string   `json:"job_id"`
	Quote   pinQuote `json:"quote"`
}

type pinQuote struct {
	Ref    string `json:"ref"`
	SHA256 string `json:"sha256"`
}

func readPin(root *os.Root, jobID string) (*quotePin, error) {
	raw, err := readRoot(root, jobID+".json", papersim.MaxInputBytes)
	if err != nil {
		return nil, err
	}
	var pin quotePin
	if contract.Decode(raw, contract.PaperQuotePin, &pin) != nil || pin.JobID != jobID {
		return nil, errors.New("invalid existing local paper quote pin")
	}
	return &pin, nil
}

func publishPin(ctx context.Context, root *os.Root, pin quotePin, raw []byte) (bool, error) {
	if err := ctx.Err(); err != nil {
		return false, err
	}
	body, err := json.Marshal(pin)
	if err != nil || contract.Decode(body, contract.PaperQuotePin, &quotePin{}) != nil {
		return false, errors.New("cannot encode local paper quote pin")
	}
	existing, err := readPin(root, pin.JobID)
	if err == nil {
		if *existing != pin {
			return false, ledger.ErrConflict
		}
		data, err := readRoot(root, pin.Quote.Ref, papersim.MaxInputBytes)
		if err != nil || !bytes.Equal(data, raw) {
			return false, errors.New("existing pinned paper quote is missing or differs")
		}
		return false, nil
	}
	if !errors.Is(err, os.ErrNotExist) {
		return false, err
	}
	quoteCreated, err := publishFile(root, pin.Quote.Ref, raw)
	if err != nil {
		return false, fmt.Errorf("publish exact paper quote bytes: %w", err)
	}
	// Link the pin only after the quote is visible. Readers never see a
	// partially written file, and neither file can overwrite existing bytes.
	created, err := publishFile(root, pin.JobID+".json", body)
	if errors.Is(err, ledger.ErrConflict) && quoteCreated {
		if cleanupErr := root.Remove(pin.Quote.Ref); cleanupErr != nil {
			return false, errors.Join(err, fmt.Errorf("remove unreferenced quote: %w", cleanupErr))
		}
	}
	return created, err
}

func publishFile(root *os.Root, name string, body []byte) (bool, error) {
	tmpName := ".paper-quote-" + rand.Text()
	tmp, err := root.OpenFile(tmpName, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return false, err
	}
	defer root.Remove(tmpName)
	_, writeErr := tmp.Write(body)
	syncErr := tmp.Sync()
	if err := errors.Join(writeErr, syncErr, tmp.Close()); err != nil {
		return false, err
	}
	if err := root.Link(tmpName, name); err != nil {
		if !errors.Is(err, os.ErrExist) {
			return false, err
		}
		existing, err := readRoot(root, name, papersim.MaxInputBytes)
		if err != nil {
			return false, err
		}
		if !bytes.Equal(body, existing) {
			return false, ledger.ErrConflict
		}
		return false, nil
	}
	return true, nil
}

func readPath(path string, limit int) ([]byte, error) {
	root, err := os.OpenRoot(filepath.Dir(path))
	if err != nil {
		return nil, err
	}
	defer root.Close()
	return readRoot(root, filepath.Base(path), limit)
}

func readRoot(root *os.Root, name string, limit int) ([]byte, error) {
	info, err := root.Stat(name)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() || info.Size() > int64(limit) {
		return nil, errors.New("paper quote input must be a bounded regular file")
	}
	file, err := root.Open(name)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	raw, err := io.ReadAll(io.LimitReader(file, int64(limit)+1))
	if err != nil {
		return nil, err
	}
	if len(raw) > limit || !utf8.Valid(raw) {
		return nil, errors.New("paper quote input must be bounded UTF-8")
	}
	return raw, nil
}
