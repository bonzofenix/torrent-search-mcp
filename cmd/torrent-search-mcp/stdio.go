package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"sync"

	"github.com/modelcontextprotocol/go-sdk/jsonrpc"
)

// maxFrame matches the SDK's own per-line limit.
const maxFrame = 16 << 20

// lockedWriter serializes writes so error replies from the frame filter never
// interleave with the SDK's responses on stdout.
type lockedWriter struct {
	mu sync.Mutex
	w  io.Writer
}

func (l *lockedWriter) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.w.Write(p)
}

func (l *lockedWriter) Close() error { return nil }

// frameFilter sits between stdin and the SDK. The SDK's stdio reader ends
// the session on the first frame it cannot decode, which would make one
// garbage line from the host kill the process. The filter forwards only
// frames the SDK accepts, one per line, answers the rest with a JSON-RPC
// error on stdout, and splits legacy batches into single messages.
type frameFilter struct {
	in      *bufio.Reader
	out     io.Writer
	pending []byte
}

func newFrameFilter(in io.Reader, out io.Writer) *frameFilter {
	return &frameFilter{in: bufio.NewReaderSize(in, 64<<10), out: out}
}

func (f *frameFilter) Read(p []byte) (int, error) {
	for len(f.pending) == 0 {
		line, err := f.in.ReadBytes('\n')
		if frame := bytes.TrimSpace(line); len(frame) > 0 {
			f.accept(frame)
		}
		if err != nil {
			if len(f.pending) == 0 {
				return 0, err
			}
			break
		}
	}
	n := copy(p, f.pending)
	f.pending = f.pending[n:]
	return n, nil
}

func (f *frameFilter) Close() error { return nil }

func (f *frameFilter) accept(frame []byte) {
	if len(frame) > maxFrame {
		f.reject(nil, -32600, fmt.Sprintf("message exceeds %d bytes", maxFrame))
		return
	}
	if !json.Valid(frame) {
		f.reject(nil, -32700, "Parse error")
		return
	}
	messages := []json.RawMessage{frame}
	if frame[0] == '[' {
		if err := json.Unmarshal(frame, &messages); err != nil || len(messages) == 0 {
			f.reject(nil, -32600, "Invalid Request: empty or malformed batch")
			return
		}
	}
	for _, msg := range messages {
		if _, err := jsonrpc.DecodeMessage(msg); err != nil {
			var probe struct {
				ID json.RawMessage `json:"id"`
			}
			_ = json.Unmarshal(msg, &probe)
			f.reject(probe.ID, -32600, "Invalid Request: "+err.Error())
			continue
		}
		var compact bytes.Buffer
		if err := json.Compact(&compact, msg); err != nil {
			continue
		}
		f.pending = append(append(f.pending, compact.Bytes()...), '\n')
	}
}

func (f *frameFilter) reject(id json.RawMessage, code int, message string) {
	log.Printf("WARNING: dropping invalid JSON-RPC frame from stdin: %s", message)
	if len(id) == 0 {
		id = json.RawMessage("null")
	}
	reply, _ := json.Marshal(map[string]any{
		"jsonrpc": "2.0",
		"id":      id,
		"error":   map[string]any{"code": code, "message": message},
	})
	_, _ = f.out.Write(append(reply, '\n'))
}
