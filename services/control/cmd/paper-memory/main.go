package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"regexp"
	"syscall"
	"time"

	"tradingbot/control/internal/contract"
	"tradingbot/control/internal/ledger"
	"tradingbot/control/internal/papermemory"
	"tradingbot/control/internal/paperrisk"
)

var archiveName = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$`)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	if err := run(ctx, os.Getenv("TRADINGBOT_DATABASE_URL"), os.Args[1:], os.Stdout); err != nil && !errors.Is(err, flag.ErrHelp) {
		slog.Error("export paper memory", "error", err)
		os.Exit(1)
	}
}

func run(ctx context.Context, dsn string, args []string, output io.Writer) error {
	if len(args) > 0 {
		switch args[0] {
		case "record-lesson", "correct-lesson", "lessons":
			return lessonCommand(ctx, dsn, args[0], args[1:], output)
		}
	}
	flags := flag.NewFlagSet("paper-memory", flag.ContinueOnError)
	flags.Usage = func() {
		fmt.Fprintln(flags.Output(), "Export a paper archive or use record-lesson, correct-lesson, or lessons.")
		flags.PrintDefaults()
	}
	account := flags.String("account", "", "paper account UUID")
	cutoff := flags.String("as-of", "", "same RFC3339 cutoff as the research portfolio")
	policyPath := flags.String("policy", "", "paper policy file for the exact instrument registry")
	inputPath := flags.String("input", "", "existing market archive file")
	inputDigest := flags.String("input-sha256", "", "verified hash of the existing market archive bytes")
	outputPath := flags.String("output", "", "new archive file, under the worker's archive root")
	durable := flags.Bool("durable-lessons", false, "include active stored lessons at the archive cutoff")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if dsn == "" || *account == "" || *policyPath == "" || *inputPath == "" || *inputDigest == "" || *outputPath == "" ||
		!archiveName.MatchString(filepath.Base(*outputPath)) || flags.NArg() != 0 {
		return errors.New("provide TRADINGBOT_DATABASE_URL, --account, --as-of, --policy, --input, --input-sha256 and --output")
	}
	asOf, err := time.Parse(time.RFC3339Nano, *cutoff)
	if err != nil || asOf.IsZero() || asOf.After(time.Now()) {
		return errors.New("paper archive cutoff must be an RFC3339 timestamp at or before the current time")
	}
	rawPolicy, err := readFile(*policyPath, 256*1024)
	if err != nil {
		return fmt.Errorf("read paper policy: %w", err)
	}
	var policy paperrisk.Policy
	if err := contract.Decode(rawPolicy, contract.PaperRiskPolicy, &policy); err != nil {
		return errors.New("invalid paper policy document")
	}
	raw, err := readFile(*inputPath, papermemory.MaxArchiveBytes)
	if err != nil {
		return fmt.Errorf("read market archive: %w", err)
	}
	var archive []byte
	if *durable {
		err = ledger.WithReadSnapshot(ctx, dsn, func(snapshot ledger.ReadSnapshot) error {
			var err error
			archive, err = papermemory.ExportArchiveWithLessons(ctx, snapshot, *account, asOf, policy, raw, *inputDigest)
			return err
		})
	} else {
		store, openErr := ledger.Open(ctx, dsn)
		if openErr != nil {
			return errors.New("paper ledger startup failed")
		}
		defer store.Close()
		archive, err = papermemory.ExportArchive(ctx, store, *account, asOf, policy, raw, *inputDigest)
	}
	if err != nil {
		return err
	}
	if err := writeArchive(*outputPath, archive); err != nil {
		return fmt.Errorf("publish paper archive: %w", err)
	}
	digest := sha256.Sum256(archive)
	return json.NewEncoder(output).Encode(map[string]string{
		"ref": filepath.Base(*outputPath), "sha256": hex.EncodeToString(digest[:]),
	})
}

func readFile(path string, limit int64) ([]byte, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() || info.Size() > limit {
		return nil, errors.New("input must be a bounded regular file")
	}
	data, err := io.ReadAll(io.LimitReader(file, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > limit {
		return nil, errors.New("input exceeds its byte limit")
	}
	return data, nil
}

func writeArchive(path string, contents []byte) error {
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return err
	}
	_, writeErr := file.Write(contents)
	if writeErr == nil {
		writeErr = file.Sync()
	}
	if err := errors.Join(writeErr, file.Close()); err != nil {
		_ = os.Remove(path)
		return err
	}
	return nil
}
