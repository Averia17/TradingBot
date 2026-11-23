// Perp-screen reads one versioned study from stdin and writes a research screen.
package main

import (
	"encoding/json"
	"fmt"
	"io"
	"os"

	"tradingbot/trading-api/internal/perp_research"
)

const maxStudyBytes = 128 * 1024

func main() {
	if err := run(os.Stdin, os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}
}

func run(input io.Reader, output io.Writer) error {
	raw, err := io.ReadAll(io.LimitReader(input, maxStudyBytes+1))
	if err != nil {
		return err
	}
	if len(raw) > maxStudyBytes {
		return fmt.Errorf("perpetual study exceeds the input limit")
	}
	screen, err := perp_research.Evaluate(raw)
	if err != nil {
		return err
	}
	return json.NewEncoder(output).Encode(screen)
}
