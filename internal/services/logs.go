package services

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/exec"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"
)

const (
	MaxLogEntries         = 100
	MaxLogMessageBytes    = 64 * 1024
	MaxJournalOutputBytes = 2 * 1024 * 1024
	MaxLogResponseBytes   = 2 * 1024 * 1024
	MaxJournalCursorBytes = 4096
)

var (
	ErrLogsUnsupported      = errors.New("systemd journal logs are unsupported on this host")
	ErrLogsUnavailable      = errors.New("systemd journal is unavailable")
	ErrLogsPermissionDenied = errors.New("permission to read the unit journal was denied")
	ErrLogCursorInvalid     = errors.New("journal cursor is malformed or belongs to another unit")
	ErrLogCursorExpired     = errors.New("journal cursor expired or rotated out")
	ErrLogsForbidden        = errors.New("unit is not allowed for log reading")
	cursorCodec             cursorCodecState
)

type cursorCodecState struct {
	once sync.Once
	aead cipher.AEAD
	err  error
}

func (c *cursorCodecState) get() (cipher.AEAD, error) {
	c.once.Do(func() {
		key := make([]byte, 32)
		defer func() {
			for index := range key {
				key[index] = 0
			}
		}()
		if _, c.err = io.ReadFull(rand.Reader, key); c.err != nil {
			return
		}
		block, err := aes.NewCipher(key)
		if err != nil {
			c.err = err
			return
		}
		c.aead, c.err = cipher.NewGCM(block)
	})
	return c.aead, c.err
}

type LogQuery struct {
	Limit  int
	Before string
	After  string
	Since  time.Time
	Until  time.Time
}

type LogEntry struct {
	Timestamp        time.Time `json:"timestamp"`
	Priority         *int      `json:"priority,omitempty"`
	Message          string    `json:"message"`
	MessageTruncated bool      `json:"message_truncated,omitempty"`
	Cursor           string    `json:"cursor"`
	rawCursor        string
}

type LogPage struct {
	Unit         string     `json:"unit"`
	Entries      []LogEntry `json:"entries"`
	NextCursor   string     `json:"next_cursor,omitempty"`
	LatestCursor string     `json:"latest_cursor,omitempty"`
	HasMore      bool       `json:"has_more,omitempty"`
}

type JournalReader interface {
	Supported() bool
	Read(context.Context, string, LogQuery) (LogPage, error)
}

type journalRunner func(context.Context, []string) ([]byte, []byte, error)

type Journal struct {
	supported bool
	run       journalRunner
}

func NewJournal() *Journal {
	_, systemdErr := os.Stat("/run/systemd/system")
	info, journalErr := os.Stat("/usr/bin/journalctl")
	return &Journal{supported: runtime.GOOS == "linux" && systemdErr == nil && journalErr == nil && info.Mode().IsRegular(), run: runJournalctl}
}

func (j *Journal) Supported() bool { return j != nil && j.supported }

// EncodeLogCursor encrypts and binds the systemd-private cursor to one unit.
// The token is opaque to clients, not an authorization token; the separate log
// bearer credential and unit allowlist are checked on every request. The key is
// process-local, so daemon restarts invalidate previously issued cursors.
func EncodeLogCursor(unit, raw string) (string, error) {
	if !ValidUnit(unit) || !validRawCursor(raw) {
		return "", ErrLogCursorInvalid
	}
	data, err := json.Marshal(struct {
		Unit   string `json:"u"`
		Cursor string `json:"c"`
	}{unit, raw})
	if err != nil {
		return "", ErrLogCursorInvalid
	}
	aead, err := cursorCodec.get()
	if err != nil {
		return "", ErrLogsUnavailable
	}
	nonce := make([]byte, aead.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return "", ErrLogsUnavailable
	}
	token := append(nonce, aead.Seal(nil, nonce, data, nil)...)
	return base64.RawURLEncoding.EncodeToString(token), nil
}

func decodeLogCursor(encoded, unit string) (string, error) {
	if len(encoded) == 0 || len(encoded) > 8192 {
		return "", ErrLogCursorInvalid
	}
	data, err := base64.RawURLEncoding.DecodeString(encoded)
	if err != nil || len(data) > MaxJournalCursorBytes+512 {
		return "", ErrLogCursorInvalid
	}
	aead, err := cursorCodec.get()
	if err != nil {
		return "", ErrLogsUnavailable
	}
	if len(data) < aead.NonceSize()+aead.Overhead() {
		return "", ErrLogCursorInvalid
	}
	nonce, ciphertext := data[:aead.NonceSize()], data[aead.NonceSize():]
	data, err = aead.Open(nil, nonce, ciphertext, nil)
	if err != nil {
		// This also covers valid tokens encrypted before a daemon restart; callers
		// can recover by requesting a fresh recent-history page.
		return "", ErrLogCursorExpired
	}
	var value struct {
		Unit   string `json:"u"`
		Cursor string `json:"c"`
	}
	if json.Unmarshal(data, &value) != nil || value.Unit != unit || !ValidUnit(value.Unit) || !validRawCursor(value.Cursor) {
		return "", ErrLogCursorInvalid
	}
	return value.Cursor, nil
}

func validRawCursor(cursor string) bool {
	if cursor == "" || len(cursor) > MaxJournalCursorBytes {
		return false
	}
	for _, character := range []byte(cursor) {
		if character < 0x21 || character > 0x7e {
			return false
		}
	}
	return true
}

func (j *Journal) Read(ctx context.Context, unit string, query LogQuery) (LogPage, error) {
	if err := ctx.Err(); err != nil {
		return LogPage{}, err
	}
	if !ValidUnit(unit) {
		return LogPage{}, ErrLogsForbidden
	}
	if !j.Supported() {
		return LogPage{}, ErrLogsUnsupported
	}
	if query.Limit == 0 {
		query.Limit = 50
	}
	if query.Limit < 1 || query.Limit > MaxLogEntries || (query.Before != "" && query.After != "") || (!query.Since.IsZero() && !query.Until.IsZero() && query.Since.After(query.Until)) {
		return LogPage{}, ErrLogsUnavailable
	}
	mode, cursor := "latest", ""
	if query.Before != "" {
		mode = "before"
		raw, err := decodeLogCursor(query.Before, unit)
		if err != nil {
			return LogPage{}, err
		}
		cursor = raw
	}
	if query.After != "" {
		mode = "after"
		raw, err := decodeLogCursor(query.After, unit)
		if err != nil {
			return LogPage{}, err
		}
		cursor = raw
	}

	args := []string{"--no-pager", "--output=json", "--unit=" + unit}
	if !query.Since.IsZero() {
		args = append(args, "--since="+query.Since.UTC().Format(time.RFC3339Nano))
	}
	if !query.Until.IsZero() {
		args = append(args, "--until="+query.Until.UTC().Format(time.RFC3339Nano))
	}
	lineCount := query.Limit + 1
	linePrefix := ""
	if mode == "before" {
		args = append(args, "--cursor="+cursor, "--reverse")
		lineCount = query.Limit + 2
	}
	if mode == "after" {
		args = append(args, "--after-cursor="+cursor)
		// journalctl's ordinary --lines=N selects the newest matching entries.
		// Prefixing N with '+' selects the oldest entries after the cursor so a
		// bounded polling page cannot skip forward over unread history.
		linePrefix = "+"
	}
	args = append(args, "--lines="+linePrefix+strconv.Itoa(lineCount))
	stdout, stderr, err := j.run(ctx, args)
	if ctx.Err() != nil {
		return LogPage{}, ctx.Err()
	}
	if hasPermissionDenial(string(stderr)) {
		return LogPage{}, ErrLogsPermissionDenied
	}
	if err != nil {
		if mode != "latest" && hasCursorFailure(string(stderr)) {
			return LogPage{}, ErrLogCursorExpired
		}
		return LogPage{}, ErrLogsUnavailable
	}
	entries, err := parseJournalEntries(stdout)
	if err != nil {
		return LogPage{}, ErrLogsUnavailable
	}

	page := LogPage{Unit: unit, Entries: []LogEntry{}}
	switch mode {
	case "latest":
		if len(entries) > query.Limit {
			page.HasMore = true
			entries = entries[len(entries)-query.Limit:]
		}
	case "before":
		older := entries[:0]
		for _, entry := range entries {
			if entry.rawCursor != cursor {
				older = append(older, entry)
			}
		}
		entries = older
		if len(entries) > query.Limit {
			page.HasMore = true
			entries = entries[:query.Limit]
		}
		reverseLogEntries(entries)
	case "after":
		if len(entries) > query.Limit {
			page.HasMore = true
			entries = entries[:query.Limit]
		}
	}
	// Preserve journalctl's ordering. Re-sorting by realtime timestamps could
	// reorder entries around a wall-clock adjustment.
	page.Entries = entries
	for index := range page.Entries {
		encoded, encodeErr := EncodeLogCursor(unit, page.Entries[index].rawCursor)
		if encodeErr != nil {
			return LogPage{}, ErrLogsUnavailable
		}
		page.Entries[index].Cursor = encoded
	}
	if len(page.Entries) > 0 {
		page.LatestCursor = page.Entries[len(page.Entries)-1].Cursor
		if page.HasMore && mode != "after" {
			page.NextCursor = page.Entries[0].Cursor
		}
	}
	return page, nil
}

func hasCursorFailure(stderr string) bool {
	message := strings.ToLower(stderr)
	return strings.Contains(message, "cursor") && (strings.Contains(message, "seek") || strings.Contains(message, "invalid") || strings.Contains(message, "not found"))
}

func reverseLogEntries(entries []LogEntry) {
	for left, right := 0, len(entries)-1; left < right; left, right = left+1, right-1 {
		entries[left], entries[right] = entries[right], entries[left]
	}
}

func parseJournalEntries(data []byte) ([]LogEntry, error) {
	if len(data) > MaxJournalOutputBytes {
		return nil, errors.New("journal output limit exceeded")
	}
	entries := make([]LogEntry, 0)
	for _, line := range strings.Split(string(data), "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		var record struct {
			Timestamp json.RawMessage `json:"__REALTIME_TIMESTAMP"`
			Priority  json.RawMessage `json:"PRIORITY"`
			Message   json.RawMessage `json:"MESSAGE"`
			Cursor    string          `json:"__CURSOR"`
		}
		if err := json.Unmarshal([]byte(line), &record); err != nil {
			return nil, err
		}
		var timestampText string
		if json.Unmarshal(record.Timestamp, &timestampText) != nil {
			timestampText = strings.TrimSpace(string(record.Timestamp))
		}
		microseconds, err := strconv.ParseInt(timestampText, 10, 64)
		if err != nil || microseconds < 0 || !validRawCursor(record.Cursor) {
			return nil, errors.New("invalid journal entry metadata")
		}
		message, err := parseJournalMessage(record.Message)
		if err != nil {
			return nil, err
		}
		entry := LogEntry{Timestamp: time.UnixMicro(microseconds).UTC(), Message: message, rawCursor: record.Cursor}
		if len(message) > MaxLogMessageBytes {
			entry.Message = truncateLogMessage(message, MaxLogMessageBytes)
			entry.MessageTruncated = true
		}
		if len(record.Priority) > 0 && string(record.Priority) != "null" {
			var priorityText string
			if json.Unmarshal(record.Priority, &priorityText) != nil {
				priorityText = strings.TrimSpace(string(record.Priority))
			}
			priority, err := strconv.Atoi(priorityText)
			if err != nil || priority < 0 || priority > 7 {
				return nil, errors.New("invalid journal priority")
			}
			entry.Priority = &priority
		}
		entries = append(entries, entry)
	}
	return entries, nil
}

func parseJournalMessage(data json.RawMessage) (string, error) {
	var message string
	if json.Unmarshal(data, &message) == nil {
		return strings.ToValidUTF8(message, "�"), nil
	}
	var binary []byte
	if json.Unmarshal(data, &binary) == nil {
		return strings.ToValidUTF8(string(binary), "�"), nil
	}
	return "", errors.New("invalid journal message")
}

func truncateLogMessage(message string, limit int) string {
	if len(message) <= limit {
		return message
	}
	end := limit
	for end > 0 && !utf8.ValidString(message[:end]) {
		end--
	}
	return message[:end]
}

type journalBuffer struct {
	data  []byte
	limit int
}

func (b *journalBuffer) Write(data []byte) (int, error) {
	if len(data) > b.limit-len(b.data) {
		return 0, errors.New("journal output limit exceeded")
	}
	b.data = append(b.data, data...)
	return len(data), nil
}

func runJournalctl(ctx context.Context, args []string) ([]byte, []byte, error) {
	command := journalctlCommand(ctx, args)
	var stdout, stderr journalBuffer
	stdout.limit, stderr.limit = MaxJournalOutputBytes, 64*1024
	command.Stdout, command.Stderr = &stdout, &stderr
	err := command.Run()
	return stdout.data, stderr.data, err
}

func journalctlCommand(ctx context.Context, args []string) *exec.Cmd {
	command := exec.CommandContext(ctx, "/usr/bin/journalctl", args...)
	command.Env = []string{"LANG=C", "LC_ALL=C", "SYSTEMD_COLORS=0", "SYSTEMD_PAGER=cat"}
	command.WaitDelay = time.Second
	return command
}
