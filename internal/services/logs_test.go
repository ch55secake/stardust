package services

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"
	"unicode/utf8"
)

func journalRecord(index int, message string) string {
	encoded, _ := json.Marshal(message)
	return fmt.Sprintf(`{"__REALTIME_TIMESTAMP":"%d","PRIORITY":"6","MESSAGE":%s,"__CURSOR":"cursor-%d"}`, 1_800_000_000_000_000+int64(index)*1_000_000, encoded, index)
}

func journalOutput(records ...string) []byte { return []byte(strings.Join(records, "\n") + "\n") }

func fakeJournal(run journalRunner) *Journal { return &Journal{supported: true, run: run} }

func TestJournalRecentAndOlderPagination(t *testing.T) {
	var captured []string
	journal := fakeJournal(func(_ context.Context, args []string) ([]byte, []byte, error) {
		captured = append([]string(nil), args...)
		return journalOutput(journalRecord(1, "one"), journalRecord(2, "two"), journalRecord(3, "three")), nil, nil
	})
	page, err := journal.Read(context.Background(), "worker.service", LogQuery{Limit: 2})
	if err != nil || len(page.Entries) != 2 || page.Entries[0].Message != "two" || page.Entries[1].Message != "three" || !page.HasMore || page.NextCursor == "" || page.LatestCursor == "" {
		t.Fatalf("recent page: %+v %v", page, err)
	}
	for _, want := range []string{"--unit=worker.service", "--output=json", "--lines=3", "--no-pager"} {
		if !slicesContains(captured, want) {
			t.Fatalf("missing safe journal argument %q: %v", want, captured)
		}
	}
	if page.Entries[0].Timestamp.Location() != time.UTC || page.Entries[0].Priority == nil || *page.Entries[0].Priority != 6 {
		t.Fatal("timestamp/priority not parsed")
	}
	olderCursor, err := EncodeLogCursor("worker.service", "cursor-3")
	if err != nil {
		t.Fatal(err)
	}
	journal.run = func(_ context.Context, args []string) ([]byte, []byte, error) {
		if !slicesContains(args, "--reverse") || !slicesContains(args, "--cursor=cursor-3") {
			t.Fatalf("older history did not use bounded reverse cursor: %v", args)
		}
		return journalOutput(journalRecord(3, "three"), journalRecord(2, "two"), journalRecord(1, "one")), nil, nil
	}
	page, err = journal.Read(context.Background(), "worker.service", LogQuery{Limit: 1, Before: olderCursor})
	if err != nil || len(page.Entries) != 1 || page.Entries[0].Message != "two" || !page.HasMore {
		t.Fatalf("older page: %+v %v", page, err)
	}
	raw, err := decodeLogCursor(page.NextCursor, "worker.service")
	if err != nil || raw != "cursor-2" {
		t.Fatalf("next older cursor %q %v", raw, err)
	}
}

func TestJournalAfterCursorPollingAndFilters(t *testing.T) {
	after, err := EncodeLogCursor("worker.service", "cursor-1")
	if err != nil {
		t.Fatal(err)
	}
	since := time.Date(2026, 10, 18, 10, 0, 0, 0, time.FixedZone("test", 3600))
	until := since.Add(time.Hour)
	var captured []string
	journal := fakeJournal(func(_ context.Context, args []string) ([]byte, []byte, error) {
		captured = append([]string(nil), args...)
		return journalOutput(journalRecord(2, "two"), journalRecord(3, "three"), journalRecord(4, "four")), nil, nil
	})
	page, err := journal.Read(context.Background(), "worker.service", LogQuery{Limit: 2, After: after, Since: since, Until: until})
	if err != nil || len(page.Entries) != 2 || page.Entries[0].Message != "two" || page.Entries[1].Message != "three" || !page.HasMore {
		t.Fatalf("follow poll: %+v %v", page, err)
	}
	raw, err := decodeLogCursor(page.LatestCursor, "worker.service")
	if err != nil || raw != "cursor-3" {
		t.Fatalf("latest cursor %q %v", raw, err)
	}
	for _, want := range []string{"--after-cursor=cursor-1", "--lines=+3", "--since=2026-10-18T09:00:00Z", "--until=2026-10-18T10:00:00Z"} {
		if !slicesContains(captured, want) {
			t.Fatalf("missing %q: %v", want, captured)
		}
	}
	journal.run = func(context.Context, []string) ([]byte, []byte, error) { return nil, nil, nil }
	page, err = journal.Read(context.Background(), "worker.service", LogQuery{Limit: 5, After: after})
	if err != nil || len(page.Entries) != 0 || page.Entries == nil {
		t.Fatalf("empty poll must be an empty array: %+v %v", page, err)
	}
}

func TestJournalPreservesOrderWhenRealtimeClockMovesBackward(t *testing.T) {
	first := journalRecord(1, "first")
	second := strings.Replace(journalRecord(2, "second"), `"__REALTIME_TIMESTAMP":"1800000000002000"`, `"__REALTIME_TIMESTAMP":"1800000000000000"`, 1)
	journal := fakeJournal(func(context.Context, []string) ([]byte, []byte, error) {
		return journalOutput(first, second), nil, nil
	})
	page, err := journal.Read(context.Background(), "worker.service", LogQuery{Limit: 5})
	if err != nil || len(page.Entries) != 2 || page.Entries[0].Message != "first" || page.Entries[1].Message != "second" {
		t.Fatalf("journal order changed after wall-clock adjustment: %+v %v", page, err)
	}
}

func TestJournalPreservesMultilineUnicodeAndBoundsMessages(t *testing.T) {
	message := "first line\nsecond café ✓\x1b[31mred\x1b[0m"
	output := journalOutput(journalRecord(1, message), journalRecord(2, strings.Repeat("界", MaxLogMessageBytes)))
	if _, parseErr := parseJournalEntries(output); parseErr != nil {
		t.Fatalf("parse fixture: %v", parseErr)
	}
	journal := fakeJournal(func(context.Context, []string) ([]byte, []byte, error) {
		return output, nil, nil
	})
	page, err := journal.Read(context.Background(), "worker.service", LogQuery{Limit: 10})
	if err != nil || len(page.Entries) != 2 {
		t.Fatalf("entries: %+v %v", page, err)
	}
	if page.Entries[0].Message != message {
		t.Fatalf("message altered at producer: %q", page.Entries[0].Message)
	}
	if !page.Entries[1].MessageTruncated || len(page.Entries[1].Message) > MaxLogMessageBytes {
		t.Fatal("oversized log message not explicitly bounded")
	}
	if !utf8.ValidString(page.Entries[1].Message) {
		t.Fatal("truncation split UTF-8")
	}
}

func TestJournalCursorScopeAndFailures(t *testing.T) {
	cursor, err := EncodeLogCursor("worker.service", "cursor-1")
	if err != nil {
		t.Fatal(err)
	}
	second, err := EncodeLogCursor("worker.service", "cursor-1")
	if err != nil || cursor == second || strings.Contains(cursor, "cursor-1") {
		t.Fatalf("cursor is not encrypted with a fresh nonce: %q %q (%v)", cursor, second, err)
	}
	for _, bad := range []string{"", "!", strings.Repeat("x", 8193)} {
		if _, err := decodeLogCursor(bad, "worker.service"); !errors.Is(err, ErrLogCursorInvalid) {
			t.Fatalf("bad cursor accepted: %v", err)
		}
	}
	if _, err := decodeLogCursor(cursor, "other.service"); !errors.Is(err, ErrLogCursorInvalid) {
		t.Fatalf("cross-unit cursor accepted: %v", err)
	}
	for _, test := range []struct {
		stderr string
		want   error
	}{
		{"Permission denied", ErrLogsPermissionDenied},
		{"No journal files were opened due to insufficient permissions.", ErrLogsPermissionDenied},
		{"Failed to seek to cursor: Invalid argument", ErrLogCursorExpired},
		{"journalctl unavailable", ErrLogsUnavailable},
	} {
		journal := fakeJournal(func(context.Context, []string) ([]byte, []byte, error) {
			return nil, []byte(test.stderr), errors.New("process exit with secret")
		})
		query := LogQuery{Limit: 5}
		if errors.Is(test.want, ErrLogCursorExpired) {
			query.Before = cursor
		}
		if _, err := journal.Read(context.Background(), "worker.service", query); !errors.Is(err, test.want) {
			t.Fatalf("got %v, want %v", err, test.want)
		}
	}
	warningJournal := fakeJournal(func(context.Context, []string) ([]byte, []byte, error) {
		return nil, []byte("No journal files were opened due to insufficient permissions."), nil
	})
	if _, err := warningJournal.Read(context.Background(), "worker.service", LogQuery{}); !errors.Is(err, ErrLogsPermissionDenied) {
		t.Fatalf("successful journalctl with permission warning returned %v", err)
	}
	journal := fakeJournal(func(context.Context, []string) ([]byte, []byte, error) { return []byte("not json\n"), nil, nil })
	if _, err := journal.Read(context.Background(), "worker.service", LogQuery{}); !errors.Is(err, ErrLogsUnavailable) {
		t.Fatal(err)
	}
	journal.supported = false
	if _, err := journal.Read(context.Background(), "worker.service", LogQuery{}); !errors.Is(err, ErrLogsUnsupported) {
		t.Fatal(err)
	}
	if _, err := journal.Read(context.Background(), "bad;unit.service", LogQuery{}); !errors.Is(err, ErrLogsForbidden) {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := fakeJournal(func(context.Context, []string) ([]byte, []byte, error) {
		t.Fatal("canceled request reached journalctl")
		return nil, nil, nil
	}).Read(ctx, "worker.service", LogQuery{}); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	started := make(chan struct{})
	running := fakeJournal(func(ctx context.Context, _ []string) ([]byte, []byte, error) {
		close(started)
		<-ctx.Done()
		return nil, nil, ctx.Err()
	})
	activeCtx, stop := context.WithCancel(context.Background())
	result := make(chan error, 1)
	go func() {
		_, err := running.Read(activeCtx, "worker.service", LogQuery{})
		result <- err
	}()
	<-started
	stop()
	if err := <-result; !errors.Is(err, context.Canceled) {
		t.Fatalf("cancellation during journal read returned %v", err)
	}
}

func TestJournalCommandIsFixedAndNoShell(t *testing.T) {
	command := journalctlCommand(context.Background(), []string{"--unit=worker.service", "--lines=51"})
	if command.Path != "/usr/bin/journalctl" || !reflect.DeepEqual(command.Args, []string{"/usr/bin/journalctl", "--unit=worker.service", "--lines=51"}) {
		t.Fatalf("unexpected command: %+v", command.Args)
	}
	if !reflect.DeepEqual(command.Env, []string{"LANG=C", "LC_ALL=C", "SYSTEMD_COLORS=0", "SYSTEMD_PAGER=cat"}) || command.WaitDelay <= 0 {
		t.Fatal("journal command inherited environment or lacks bounded cleanup")
	}
	if strings.Contains(strings.Join(command.Args, " "), ";touch") {
		t.Fatal("shell input appeared in command")
	}
}

func slicesContains(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}
