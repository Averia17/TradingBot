// Equity-study builds a route study from a versioned archive request.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"

	"tradingbot/venue-quotes/internal/equity_study"
)

const maxInputBytes = 32 * 1024 * 1024

func main() {
	input := flag.String("input", "", "equity study inputs and archived observations as JSON")
	flag.Parse()
	if *input == "" || flag.NArg() != 0 {
		flag.Usage()
		os.Exit(2)
	}
	if err := run(*input, os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}
}

func run(path string, output io.Writer) error {
	file, err := os.Open(path)
	if err != nil {
		return err
	}
	defer file.Close()
	raw, err := io.ReadAll(io.LimitReader(file, maxInputBytes+1))
	if err != nil {
		return err
	}
	if len(raw) > maxInputBytes {
		return fmt.Errorf("study input exceeds the byte limit")
	}
	result, err := equity_study.Build(raw)
	if err != nil {
		return err
	}
	return json.NewEncoder(output).Encode(result)
}
