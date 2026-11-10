package tray

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
)

// hardLineCap bounds a single logical line while resynchronising.
// Normal frames are tiny; anything near this is a bug, never business data.
const hardLineCap = 64 << 20

// Reader incrementally parses NDJSON frames from a byte stream.
// Every frame ends with '\n', so after any error the next call resumes at
// a frame boundary: oversized or corrupt lines are reported as FrameError
// and skipped, the stream itself stays usable. Only io.EOF (with no pending
// bytes) means the peer went away.
type Reader struct {
	r *bufio.Reader
}

// NewReader wraps r (typically a pipe or stdout).
func NewReader(r *bufio.Reader) *Reader { return &Reader{r: r} }

// Next returns the next frame. A *FrameError reports one bad frame and the
// caller should continue reading. Any other non-nil error (including
// io.EOF) ends the stream.
func (fr *Reader) Next() (Message, error) {
	var line []byte
	for {
		chunk, err := fr.r.ReadBytes('\n')
		if len(line)+len(chunk) > hardLineCap {
			// Hostile or broken peer: discard through the newline so the
			// stream stays aligned, then report and continue.
			if !endsWithNewline(chunk) {
				if derr := discardUntilNewline(fr.r); derr != nil {
					return Message{}, derr
				}
			}
			return Message{}, &FrameError{Code: ErrFrameTooLarge, Err: fmt.Errorf("tray: frame exceeds hard limit %d", hardLineCap)}
		}
		line = append(line, chunk...)
		if endsWithNewline(chunk) {
			break
		}
		if err != nil {
			break
		}
	}
	if len(line) == 0 {
		return Message{}, io.EOF
	}
	if len(line) > MaxFrameBytes {
		return Message{}, &FrameError{Code: ErrFrameTooLarge, Err: fmt.Errorf("tray: frame exceeds %d bytes", MaxFrameBytes)}
	}
	m, err := Decode(line)
	if err != nil {
		return Message{}, &FrameError{Code: ErrBadFrame, Err: err}
	}
	return m, nil
}

func endsWithNewline(b []byte) bool {
	return len(b) > 0 && b[len(b)-1] == '\n'
}

func discardUntilNewline(r *bufio.Reader) error {
	for {
		chunk, err := r.ReadBytes('\n')
		_ = chunk
		if endsWithNewline(chunk) {
			return nil
		}
		if err != nil {
			return err
		}
	}
}

// FrameError is a transport-level failure for one frame. The stream itself
// stays usable; callers must not mistake it for a business error.
type FrameError struct {
	Code string
	Err  error
}

func (e *FrameError) Error() string { return e.Err.Error() }

// Encode serializes one message as a single NDJSON line.
func Encode(m Message) ([]byte, error) {
	raw, err := json.Marshal(m)
	if err != nil {
		return nil, err
	}
	raw = append(raw, '\n')
	if len(raw) > MaxFrameBytes {
		return nil, fmt.Errorf("tray: frame too large (%d bytes)", len(raw))
	}
	return raw, nil
}

// Decode parses one NDJSON line (trailing newline optional).
func Decode(line []byte) (Message, error) {
	var m Message
	line = bytes.TrimRight(line, "\r\n")
	if len(line) == 0 {
		return m, fmt.Errorf("tray: empty frame")
	}
	dec := json.NewDecoder(bytes.NewReader(line))
	// Unknown fields are ignored for forward compatibility; unknown
	// message *types* are ignored by the dispatcher, never fatal.
	if err := dec.Decode(&m); err != nil {
		return m, fmt.Errorf("tray: bad frame: %w", err)
	}
	if m.Type == "" {
		return m, fmt.Errorf("tray: frame missing type")
	}
	return m, nil
}
