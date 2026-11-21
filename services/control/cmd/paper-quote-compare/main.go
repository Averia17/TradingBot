package main

import (
	"encoding/json"
	"errors"
	"flag"
	"io"
	"log/slog"
	"os"

	"tradingbot/control/internal/paper_quotes"
)

func main() {
	input := flag.String("input", "", "versioned synthetic quote comparison JSON")
	flag.Parse()
	if err := run(*input, flag.NArg(), os.Stdout); err != nil {
		slog.Error("compare synthetic quotes", "error", err)
		os.Exit(1)
	}
}

func run(path string, positionalArgs int, output io.Writer) error {
	if path == "" || positionalArgs != 0 {
		return errors.New("provide one synthetic comparison file through --input")
	}
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() || info.Size() > paper_quotes.MaxInputBytes {
		return errors.New("comparison input must be a bounded regular file")
	}
	data, err := io.ReadAll(io.LimitReader(f, paper_quotes.MaxInputBytes+1))
	if err != nil {
		return err
	}
	result, err := paper_quotes.Compare(data)
	if err != nil {
		return err
	}
	return json.NewEncoder(output).Encode(result)
}
