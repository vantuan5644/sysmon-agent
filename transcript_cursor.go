package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"hash/crc32"
	"io"
	"os"
)

// Incremental reading of append-only transcript JSONL.
//
// The token charts used to re-decode every transcript written in the last seven
// days on every 30 s pass. On 2026-11-18 that was ~900 MB per pass on BBLWIN
// (one Codex session alone was 264 MB): a full core for ~3.5 s every 30 s,
// which lifted Tctl ~20 C each time and set the case fans cycling. A transcript
// only ever grows, so a pass now resumes each file where the previous one
// stopped and parses just the complete lines added since.
//
// A file that shrank, or whose first or last consumed bytes changed, was
// rewritten rather than appended to; its cursor resets and it is read again from
// the start. The fingerprints are checked on every pass because a same-size
// rewrite can keep the same mtime on filesystems with coarse timestamps.

const transcriptFingerprintBytes = 4096

// transcriptReaderBuffer bounds the per-call read buffer; longer lines (large
// tool outputs) are assembled in a scratch slice that dies with the call.
const transcriptReaderBuffer = 256 << 10

type transcriptCursor struct {
	offset  int64 // bytes consumed: the end of the last complete record
	headLen int64
	headSum uint32
	tailLen int64
	tailSum uint32
}

// readNewTranscriptLines hands each complete record appended to path since cur
// to each, then advances cur. When the file was rewritten it calls reset first,
// so the caller drops what it derived from the old contents, and reads it all.
// each must not keep the slice it is given.
func readNewTranscriptLines(path string, cur *transcriptCursor, reset func(), each func([]byte)) error {
	file, err := os.Open(path)
	if err != nil {
		return err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return err
	}
	if cur.offset > 0 {
		same, err := cur.matches(file, info.Size())
		if err != nil {
			return err
		}
		if !same {
			*cur = transcriptCursor{}
			reset()
		}
	}
	if info.Size() == cur.offset {
		return nil
	}
	if _, err := file.Seek(cur.offset, io.SeekStart); err != nil {
		return err
	}
	consumed, err := readCompleteRecords(bufio.NewReaderSize(file, transcriptReaderBuffer), each)
	if err != nil {
		return err
	}
	if consumed == 0 {
		return nil
	}
	return cur.advance(file, cur.offset+consumed)
}

// readCompleteRecords passes every newline-terminated line to each and returns
// the bytes consumed. The unterminated tail of an actively written file is left
// for a later pass, unless it already holds one complete JSON value: a writer
// that ended its last record without a newline must still be counted.
func readCompleteRecords(reader *bufio.Reader, each func([]byte)) (int64, error) {
	var consumed int64
	var long []byte
	for {
		chunk, err := reader.ReadSlice('\n')
		if errors.Is(err, bufio.ErrBufferFull) {
			long = append(long, chunk...)
			continue
		}
		line := chunk
		if long != nil {
			long = append(long, chunk...)
			line = long
		}
		switch {
		case err == nil:
			consumed += int64(len(line))
			each(line)
			long = nil
		case errors.Is(err, io.EOF):
			if len(bytes.TrimSpace(line)) > 0 && json.Valid(line) {
				consumed += int64(len(line))
				each(line)
			}
			return consumed, nil
		default:
			return consumed, err
		}
	}
}

// matches reports whether the file still starts and continues with the bytes
// this cursor consumed.
func (c *transcriptCursor) matches(file *os.File, size int64) (bool, error) {
	if size < c.offset {
		return false, nil
	}
	for _, part := range []struct {
		from, length int64
		sum          uint32
	}{
		{0, c.headLen, c.headSum},
		{c.offset - c.tailLen, c.tailLen, c.tailSum},
	} {
		sum, err := checksumRange(file, part.from, part.length)
		if errors.Is(err, io.EOF) {
			return false, nil // shorter than what was consumed: rewritten
		}
		if err != nil {
			return false, err
		}
		if sum != part.sum {
			return false, nil
		}
	}
	return true, nil
}

func (c *transcriptCursor) advance(file *os.File, offset int64) error {
	fingerprint := min(offset, int64(transcriptFingerprintBytes))
	if fingerprint != c.headLen {
		sum, err := checksumRange(file, 0, fingerprint)
		if err != nil {
			return err
		}
		c.headLen, c.headSum = fingerprint, sum
	}
	sum, err := checksumRange(file, offset-fingerprint, fingerprint)
	if err != nil {
		return err
	}
	c.tailLen, c.tailSum = fingerprint, sum
	c.offset = offset
	return nil
}

// checksumRange returns io.EOF when the file ends before from+length.
func checksumRange(file *os.File, from, length int64) (uint32, error) {
	if length <= 0 {
		return 0, nil
	}
	buf := make([]byte, length)
	if _, err := file.ReadAt(buf, from); err != nil {
		return 0, err
	}
	return crc32.ChecksumIEEE(buf), nil
}
