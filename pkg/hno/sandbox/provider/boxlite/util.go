package boxlite

import (
	"sync"

	"github.com/rexleimo/agno-go/pkg/hno/sandbox"
)

// cappedBuffer accumulates a stream up to a limit and reports truncation instead of
// growing without bound.
type cappedBuffer struct {
	mu        sync.Mutex
	buf       []byte
	limit     int
	written   int64
	truncated bool
}

func (b *cappedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.written += int64(len(p))
	if b.limit > 0 {
		if room := b.limit - len(b.buf); room <= 0 {
			b.truncated = true
			return len(p), nil
		} else if room < len(p) {
			b.buf = append(b.buf, p[:room]...)
			b.truncated = true
			return len(p), nil
		}
	}
	b.buf = append(b.buf, p...)
	return len(p), nil
}

func (b *cappedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.truncated {
		return string(b.buf) + "\n[boxlite: output truncated at the capture ceiling]"
	}
	return string(b.buf)
}

// Truncated reports whether the ceiling was hit. Callers that must not present a
// clipped payload as complete check it rather than comparing lengths.
func (b *cappedBuffer) Truncated() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.truncated
}

// streamWriter delivers chunks to the sink hook while capturing them locally, so
// "this provider streams" is a fact about bytes handed over, not a declaration.
type streamWriter struct {
	buf      *cappedBuffer
	stream   sandbox.Stream
	onChunk  func(sandbox.Stream, []byte)
	streamed int64
	mu       sync.Mutex
}

func (w *streamWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	w.streamed += int64(len(p))
	w.mu.Unlock()
	if w.onChunk != nil {
		chunk := make([]byte, len(p))
		copy(chunk, p)
		w.onChunk(w.stream, chunk)
	}
	return w.buf.Write(p)
}

func (w *streamWriter) Streamed() int64 {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.streamed
}
