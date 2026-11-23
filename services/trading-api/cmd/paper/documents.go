package main

import (
	"errors"
	"io"
	"os"
	"unicode/utf8"

	"github.com/santhosh-tekuri/jsonschema/v6"

	"tradingbot/trading-api/internal/contract"
)

const maxDocumentBytes = 256 * 1024

func decodeDocument(path string, validator *jsonschema.Schema, target any) error {
	data, err := readDocument(path)
	if err != nil {
		return err
	}
	return contract.Decode(data, validator, target)
}

func readDocument(path string) ([]byte, error) {
	input, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer input.Close()
	return readPaperDocument(input)
}

func readPaperDocument(input *os.File) ([]byte, error) {
	info, err := input.Stat()
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() || info.Size() > maxDocumentBytes {
		return nil, errors.New("paper document must be a bounded regular file")
	}
	data, err := io.ReadAll(io.LimitReader(input, maxDocumentBytes+1))
	if err != nil {
		return nil, err
	}
	if len(data) > maxDocumentBytes || !utf8.Valid(data) {
		return nil, errors.New("paper document exceeds the size limit or is not valid UTF-8")
	}
	return data, nil
}
