package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"unicode/utf8"

	"tradingbot/control/internal/ledger"
	"tradingbot/control/internal/paper_quotes"
	"tradingbot/control/internal/paper_sim"
)

func readQuotePin(root *os.Root, jobID string) (*paper_quotes.QuotePin, error) {
	raw, err := readRoot(root, jobID+".json", paper_sim.MaxInputBytes)
	if err != nil {
		return nil, err
	}
	pin, err := paper_quotes.DecodeQuotePin(raw, jobID)
	if err != nil {
		return nil, errors.New("invalid existing local paper quote pin")
	}
	return &pin, nil
}

func publishQuotePin(ctx context.Context, root *os.Root, pin paper_quotes.QuotePin, raw []byte) (bool, error) {
	if err := ctx.Err(); err != nil {
		return false, err
	}
	body, err := json.Marshal(pin)
	if err != nil {
		return false, fmt.Errorf("encode local paper quote pin: %w", err)
	}
	if _, err := paper_quotes.DecodeQuotePin(body, pin.JobID); err != nil {
		return false, err
	}
	existing, err := readQuotePin(root, pin.JobID)
	if err == nil {
		if *existing != pin {
			return false, ledger.ErrConflict
		}
		data, err := readRoot(root, pin.Quote.Ref, paper_sim.MaxInputBytes)
		if err != nil || !bytes.Equal(data, raw) {
			return false, errors.New("existing pinned paper quote is missing or differs")
		}
		return false, nil
	}
	if !errors.Is(err, os.ErrNotExist) {
		return false, err
	}
	quoteCreated, err := publishOrMatchFile(root, pin.Quote.Ref, raw)
	if err != nil {
		return false, fmt.Errorf("publish exact paper quote bytes: %w", err)
	}
	// Link the pin only after the quote is visible. Readers never see a
	// partially written file, and neither file can overwrite existing bytes.
	created, err := publishOrMatchFile(root, pin.JobID+".json", body)
	if errors.Is(err, ledger.ErrConflict) && quoteCreated {
		if cleanupErr := root.Remove(pin.Quote.Ref); cleanupErr != nil {
			return false, errors.Join(err, fmt.Errorf("remove unreferenced quote: %w", cleanupErr))
		}
	}
	return created, err
}

func publishOrMatchFile(root *os.Root, name string, body []byte) (bool, error) {
	if err := paper_quotes.PublishNewFile(root, name, body); err != nil {
		if !errors.Is(err, os.ErrExist) {
			return false, err
		}
		existing, err := readRoot(root, name, paper_sim.MaxInputBytes)
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
