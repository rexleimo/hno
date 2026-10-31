package sandbox

import (
	"context"
	"time"
)

// Stream identifies which output channel a chunk belongs to.
type Stream uint8

const (
	// StreamStdout is standard output.
	StreamStdout Stream = iota
	// StreamStderr is standard error.
	StreamStderr
	// StreamPTY is the interactive terminal stream (requires CapPTY).
	StreamPTY
)

// String names the stream.
func (s Stream) String() string {
	switch s {
	case StreamStdout:
		return "stdout"
	case StreamStderr:
		return "stderr"
	default:
		return "pty"
	}
}

// OutputChunk is one piece of streamed output. Sequence is monotonic per run so
// a reattach can prove it did not lose or duplicate bytes (§15).
type OutputChunk struct {
	RunID     string
	Stream    Stream
	Bytes     []byte
	Sequence  int64
	At        time.Time
	Truncated bool
}

// Sink receives execution output: UI, log shipper, issue comment. A Sink is
// never consulted for permission, and the core never queues without bound:
// a slow consumer is reported as a fault, it does not stall or silently drop
// the run (§7).
type Sink interface {
	Emit(ctx context.Context, chunk OutputChunk) error
}

// SinkFunc adapts a function to Sink.
type SinkFunc func(ctx context.Context, chunk OutputChunk) error

// Emit implements Sink.
func (f SinkFunc) Emit(ctx context.Context, chunk OutputChunk) error { return f(ctx, chunk) }

// OverflowPolicy decides what happens when a Sink cannot keep up.
type OverflowPolicy uint8

const (
	// OverflowUnset resolves to OverflowDrop, the non-blocking default.
	OverflowUnset OverflowPolicy = iota
	// OverflowDrop discards the newest chunk, marks the report Truncated and
	// counts the loss. Execution is never stalled by a consumer.
	OverflowDrop
	// OverflowBlock applies back-pressure to the run. Only valid for short runs
	// where losing output is worse than being slow.
	OverflowBlock
	// OverflowFault stops the run with a HookFaultError. Use when output is the
	// product (streaming UIs that must not gap).
	OverflowFault
)

// String names the overflow policy.
func (p OverflowPolicy) String() string {
	switch p {
	case OverflowBlock:
		return "block"
	case OverflowFault:
		return "fault"
	default:
		return "drop"
	}
}

// SinkBinding pairs a sink with its own queue and overflow behaviour so one
// slow consumer cannot degrade a faster one (§7).
type SinkBinding struct {
	Name       string
	Sink       Sink
	Buffered   int
	OnOverflow OverflowPolicy
}
