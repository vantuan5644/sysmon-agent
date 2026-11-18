package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func appendTranscript(t *testing.T, path, contents string) {
	t.Helper()
	file, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	if _, err := file.WriteString(contents); err != nil {
		t.Fatal(err)
	}
}

type lineLog struct {
	lines  []string
	resets int
}

func (l *lineLog) read(t *testing.T, path string, cur *transcriptCursor) []string {
	t.Helper()
	before := len(l.lines)
	if err := readNewTranscriptLines(path, cur, func() { l.resets++ }, func(line []byte) {
		l.lines = append(l.lines, strings.TrimRight(string(line), "\r\n"))
	}); err != nil {
		t.Fatal(err)
	}
	return l.lines[before:]
}

func TestReadNewTranscriptLinesResumesAfterLastCompleteLine(t *testing.T) {
	path := filepath.Join(t.TempDir(), "session.jsonl")
	writeTranscript(t, path, `{"n":1}`+"\n"+`{"n":2}`+"\n"+`{"n":`)
	var cur transcriptCursor
	var log lineLog
	if got := log.read(t, path, &cur); len(got) != 2 || got[1] != `{"n":2}` {
		t.Fatalf("first pass = %q", got)
	}
	if got := log.read(t, path, &cur); len(got) != 0 {
		t.Fatalf("unchanged pass = %q, want nothing", got)
	}
	appendTranscript(t, path, `3}`+"\n"+`{"n":4}`+"\n")
	if got := log.read(t, path, &cur); len(got) != 2 || got[0] != `{"n":3}` || got[1] != `{"n":4}` {
		t.Fatalf("append pass = %q", got)
	}
	if log.resets != 0 {
		t.Fatalf("resets = %d, want 0", log.resets)
	}
}

func TestReadNewTranscriptLinesTakesCompleteUnterminatedRecord(t *testing.T) {
	path := filepath.Join(t.TempDir(), "session.jsonl")
	writeTranscript(t, path, `{"n":1}`)
	var cur transcriptCursor
	var log lineLog
	if got := log.read(t, path, &cur); len(got) != 1 {
		t.Fatalf("first pass = %q", got)
	}
	appendTranscript(t, path, "\n"+`{"n":2}`+"\n")
	got := log.read(t, path, &cur)
	if len(got) != 2 || got[0] != "" || got[1] != `{"n":2}` {
		t.Fatalf("second pass = %q, want the bare newline then n=2", got)
	}
}

func TestReadNewTranscriptLinesRestartsRewrittenFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "session.jsonl")
	writeTranscript(t, path, `{"n":1}`+"\n"+`{"n":2}`+"\n")
	var cur transcriptCursor
	var log lineLog
	log.read(t, path, &cur)

	// Same size, different bytes: the size alone cannot tell.
	writeTranscript(t, path, `{"n":7}`+"\n"+`{"n":8}`+"\n")
	if got := log.read(t, path, &cur); log.resets != 1 || len(got) != 2 || got[0] != `{"n":7}` {
		t.Fatalf("same-size rewrite: resets=%d lines=%q", log.resets, got)
	}
	writeTranscript(t, path, `{"n":9}`+"\n")
	if got := log.read(t, path, &cur); log.resets != 2 || len(got) != 1 || got[0] != `{"n":9}` {
		t.Fatalf("shrunk file: resets=%d lines=%q", log.resets, got)
	}
	// Longer than before but the consumed prefix changed.
	writeTranscript(t, path, `{"n":5}`+"\n"+`{"n":6}`+"\n")
	if got := log.read(t, path, &cur); log.resets != 3 || len(got) != 2 || got[0] != `{"n":5}` {
		t.Fatalf("grown rewrite: resets=%d lines=%q", log.resets, got)
	}
}

func TestReadNewTranscriptLinesAssemblesLinesLongerThanBuffer(t *testing.T) {
	path := filepath.Join(t.TempDir(), "session.jsonl")
	long := `{"pad":"` + strings.Repeat("x", 3*transcriptReaderBuffer) + `"}`
	writeTranscript(t, path, long+"\n"+`{"n":1}`+"\n")
	var cur transcriptCursor
	var log lineLog
	got := log.read(t, path, &cur)
	if len(got) != 2 || got[0] != long || got[1] != `{"n":1}` {
		t.Fatalf("lines = %d, first len %d", len(got), len(got[0]))
	}
	if info, _ := os.Stat(path); cur.offset != info.Size() {
		t.Fatalf("offset = %d, want %d", cur.offset, info.Size())
	}
}

func claudeUsageLine(at time.Time, request string, tokens int64) string {
	return fmt.Sprintf(`{"type":"assistant","timestamp":%q,"requestId":%q,"message":{"id":"m-%s","role":"assistant","usage":{"input_tokens":%d,"output_tokens":0}}}`+"\n", at.UTC().Format(time.RFC3339Nano), request, request, tokens)
}

func TestClaudeUsageScannerMatchesFullScanAcrossAppends(t *testing.T) {
	dir := t.TempDir()
	now := time.Now().Truncate(time.Second)
	a := filepath.Join(dir, "projects", "p", "a.jsonl")
	b := filepath.Join(dir, "projects", "p", "b.jsonl")
	writeTranscript(t, a, claudeUsageLine(now.Add(-26*time.Hour), "r1", 10)+claudeUsageLine(now.Add(-26*time.Hour), "r1", 10)+`{"type":"assist`)
	writeTranscript(t, b, claudeUsageLine(now.Add(-3*time.Hour), "r2", 20))

	var scanner claudeUsageScanner
	check := func(stage string) {
		t.Helper()
		got, gotNewest, err := scanner.scan(dir, now)
		if err != nil {
			t.Fatal(err)
		}
		want, wantNewest, err := scanClaudeTokenUsage(dir, now)
		if err != nil {
			t.Fatal(err)
		}
		if fmt.Sprint(got) != fmt.Sprint(want) || !gotNewest.Equal(wantNewest) {
			t.Fatalf("%s: incremental %v %s, full %v %s", stage, got, gotNewest, want, wantNewest)
		}
	}
	check("first")
	if tokenDaysTotal(mustClaudeDays(t, &scanner, dir, now)) != 30 {
		t.Fatal("first pass total, want 30")
	}
	// Finish a's tail, repeat a request b already holds, and add a new one.
	appendTranscript(t, a, `ant"}`+"\n"+claudeUsageLine(now.Add(-3*time.Hour), "r2", 20)+claudeUsageLine(now.Add(-time.Hour), "r3", 5))
	check("after append")
	if tokenDaysTotal(mustClaudeDays(t, &scanner, dir, now)) != 35 {
		t.Fatal("total after append, want 35")
	}
	if err := os.Remove(b); err != nil {
		t.Fatal(err)
	}
	check("after delete")
	if len(scanner.files) != 1 {
		t.Fatalf("tracked files = %d, want 1 after delete", len(scanner.files))
	}
	// Six days on, r1 has left the window and its record is dropped.
	later := now.Add(6 * 24 * time.Hour)
	mustClaudeDays(t, &scanner, dir, later)
	for _, record := range scanner.files[a].records {
		if record.key == "r1" {
			t.Fatalf("r1 kept after leaving the window: %+v", scanner.files[a].records)
		}
	}
}

func mustClaudeDays(t *testing.T, scanner *claudeUsageScanner, dir string, now time.Time) []TokenUsageDay {
	t.Helper()
	days, _, err := scanner.scan(dir, now)
	if err != nil {
		t.Fatal(err)
	}
	return days
}

func codexCountLine(at time.Time, total int64, limits string) string {
	if limits == "" {
		limits = "null"
	}
	return fmt.Sprintf(`{"timestamp":%q,"type":"event_msg","payload":{"type":"token_count","info":{"total_token_usage":{"total_tokens":%d}},"rate_limits":%s}}`+"\n", at.UTC().Format(time.RFC3339Nano), total, limits)
}

func TestCodexUsageScannerKeepsCounterAcrossPasses(t *testing.T) {
	dir := t.TempDir()
	now := time.Now().Truncate(time.Second)
	path := filepath.Join(dir, "sessions", "2026", "11", "18", "rollout.jsonl")
	first := `{"primary":{"used_percent":10,"window_minutes":10080,"resets_at":1893456000}}`
	second := `{"primary":{"used_percent":12,"window_minutes":10080,"resets_at":1893456000}}`
	writeTranscript(t, path, codexCountLine(now.Add(-2*time.Hour), 100, first)+codexCountLine(now.Add(-2*time.Hour+time.Minute), 150, ""))

	var scanner codexUsageScanner
	check := func(stage string, wantTotal int64, wantPercent float64) {
		t.Helper()
		got, err := scanner.scan(dir, now)
		if err != nil {
			t.Fatal(err)
		}
		want, err := scanCodexUsage(dir, now)
		if err != nil {
			t.Fatal(err)
		}
		if fmt.Sprint(got) != fmt.Sprint(want) {
			t.Fatalf("%s: incremental %+v, full %+v", stage, got, want)
		}
		if total := tokenDaysTotal(got.Days); total != wantTotal {
			t.Fatalf("%s: total = %d, want %d", stage, total, wantTotal)
		}
		if len(got.Rows) != 1 || got.Rows[0].Percent != wantPercent {
			t.Fatalf("%s: rows = %+v, want %.0f%%", stage, got.Rows, wantPercent)
		}
	}
	check("first", 150, 10)
	// The next pass starts mid-file; the step must be 175-150, not 175.
	appendTranscript(t, path, codexCountLine(now.Add(-time.Hour), 175, second))
	check("after append", 175, 12)
	check("unchanged", 175, 12)
}

func BenchmarkClaudeUsageScannerUnchanged(b *testing.B) {
	dir := b.TempDir()
	now := time.Now()
	var contents strings.Builder
	for i := 0; i < 20000; i++ {
		contents.WriteString(claudeUsageLine(now.Add(-time.Duration(i)*time.Second), fmt.Sprintf("r%d", i), 10))
	}
	path := filepath.Join(dir, "projects", "p", "s.jsonl")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		b.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(contents.String()), 0o644); err != nil {
		b.Fatal(err)
	}
	var scanner claudeUsageScanner
	if _, _, err := scanner.scan(dir, now); err != nil {
		b.Fatal(err)
	}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, _, err := scanner.scan(dir, now); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkClaudeUsageFullScan(b *testing.B) {
	dir := b.TempDir()
	now := time.Now()
	var contents strings.Builder
	for i := 0; i < 20000; i++ {
		contents.WriteString(claudeUsageLine(now.Add(-time.Duration(i)*time.Second), fmt.Sprintf("r%d", i), 10))
	}
	path := filepath.Join(dir, "projects", "p", "s.jsonl")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		b.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(contents.String()), 0o644); err != nil {
		b.Fatal(err)
	}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, _, err := scanClaudeTokenUsage(dir, now); err != nil {
			b.Fatal(err)
		}
	}
}
