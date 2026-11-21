package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"io"
	"time"

	"tradingbot/control/internal/ledger"
	"tradingbot/control/internal/paper_memory"
)

func lessonCommand(ctx context.Context, dsn, command string, args []string, output io.Writer) error {
	flags := flag.NewFlagSet("paper-memory "+command, flag.ContinueOnError)
	account := flags.String("account", "", "paper account UUID")
	var fill, event, id, action, cutoff string
	var expected, limit int
	var history bool
	switch command {
	case "record-lesson":
		flags.StringVar(&fill, "fill-event", "", "realized SELL fill event UUID")
		flags.StringVar(&event, "event", "", "stable operation UUID")
	case "correct-lesson":
		flags.StringVar(&id, "lesson", "", "lesson ID")
		flags.StringVar(&event, "event", "", "stable operation UUID")
		flags.StringVar(&action, "action", "", "retract or reinstate")
		flags.IntVar(&expected, "expected-revision", 0, "current lesson revision")
	case "lessons":
		flags.StringVar(&id, "lesson", "", "filter by exact lesson ID")
		flags.StringVar(&cutoff, "as-of", "", "RFC3339 availability cutoff, defaults to database time")
		flags.BoolVar(&history, "history", false, "include earlier visible revisions")
		flags.IntVar(&limit, "limit", 100, "maximum visible records, between 1 and 100")
	}
	if err := flags.Parse(args); err != nil {
		return err
	}
	if dsn == "" || *account == "" || flags.NArg() != 0 {
		return errors.New("provide TRADINGBOT_DATABASE_URL and --account")
	}
	if command == "record-lesson" && (fill == "" || event == "") {
		return errors.New("provide --fill-event and --event")
	}
	if command == "correct-lesson" && (id == "" || event == "" || action == "" || expected < 1) {
		return errors.New("provide --lesson, --event, --action and --expected-revision")
	}
	var asOf time.Time
	if cutoff != "" {
		var err error
		asOf, err = time.Parse(time.RFC3339Nano, cutoff)
		if err != nil || asOf.IsZero() {
			return errors.New("lesson cutoff must be an RFC3339 timestamp")
		}
	}
	store, err := ledger.Open(ctx, dsn)
	if err != nil {
		return errors.New("paper ledger startup failed")
	}
	defer store.Close()
	var result any
	switch command {
	case "record-lesson":
		var fact ledger.LessonFact
		err = ledger.WithReadSnapshot(ctx, dsn, func(snapshot ledger.ReadSnapshot) error {
			var err error
			fact, err = paper_memory.BuildLesson(ctx, snapshot, *account, fill, snapshot.Cutoff())
			return err
		})
		if err != nil {
			return err
		}
		result, err = store.RecordLesson(ctx, fact, event)
	case "correct-lesson":
		result, err = store.CorrectLesson(ctx, *account, id, event, action, expected)
	case "lessons":
		result, err = store.Lessons(ctx, *account, id, asOf, history, limit)
	}
	if err != nil {
		return err
	}
	return json.NewEncoder(output).Encode(result)
}
