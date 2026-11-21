// Equity-route evaluates one normalized quote study and a bound thesis batch.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"

	"tradingbot/control/internal/equity_route"
)

const maxInputBytes = 256 * 1024

func main() {
	study := flag.String("study", "", "normalized equity study JSON file")
	theses := flag.String("theses", "", "thesis batch JSON file; omit to inspect the study")
	flag.Parse()
	if *study == "" || flag.NArg() != 0 {
		flag.Usage()
		os.Exit(2)
	}
	if err := run(*study, *theses, os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}
}

func run(studyPath, thesisPath string, output io.Writer) error {
	raw, err := readInput(studyPath)
	if err != nil {
		return err
	}
	study, err := equity_route.LoadStudy(raw)
	if err != nil {
		return err
	}
	encoder := json.NewEncoder(output)
	if thesisPath == "" {
		return encoder.Encode(study.Info())
	}
	raw, err = readInput(thesisPath)
	if err != nil {
		return err
	}
	decision, err := study.Decide(raw)
	if err != nil {
		return err
	}
	return encoder.Encode(decision)
}

func readInput(path string) ([]byte, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	raw, err := io.ReadAll(io.LimitReader(file, maxInputBytes+1))
	if err != nil {
		return nil, err
	}
	if len(raw) > maxInputBytes {
		return nil, fmt.Errorf("equity route input exceeds the byte limit")
	}
	return raw, nil
}
