package mcp

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"os/exec"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/potent/potent/internal/pipeline"
	"github.com/potent/potent/internal/policy"
	"github.com/potent/potent/internal/store"
)

// fakeMCPServer is a tiny in-process MCP server that responds to tools/call
// frames with a deterministic result and counts how many were received.
type fakeMCPServer struct {
	mu    sync.Mutex
	calls int
}

func (f *fakeMCPServer) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

func (f *fakeMCPServer) handle(in io.Reader, out io.Writer) {
	dec := json.NewDecoder(in)
	for {
		var msg Message
		if err := dec.Decode(&msg); err != nil {
			return
		}
		if msg.Method != MethodToolsCall {
			// echo non-tools/call as success
			resp := Message{JSONRPC: "2.0", ID: msg.ID, Result: json.RawMessage(`{}`)}
			b, _ := json.Marshal(resp)
			_, _ = out.Write(b)
			_, _ = out.Write([]byte{'\n'})
			continue
		}
		f.mu.Lock()
		f.calls++
		count := f.calls
		f.mu.Unlock()

		body, _ := json.Marshal(map[string]any{"call": count, "ok": true})
		resp := Message{JSONRPC: "2.0", ID: msg.ID, Result: body}
		b, _ := json.Marshal(resp)
		_, _ = out.Write(b)
		_, _ = out.Write([]byte{'\n'})
	}
}

// TestStdio_ToolsCallReplaysFromCache verifies that a duplicate tools/call
// is served from cache without reaching the upstream child.
//
// Rather than spawn a real child process (which would require building a
// helper binary), we drive StdioHandler's pump methods directly with
// io.Pipe pairs and an in-process fake server.
func TestStdio_ToolsCallReplaysFromCache(t *testing.T) {
	fake := &fakeMCPServer{}

	clientIn, clientInW := io.Pipe()    // we write into the proxy's "stdin"
	clientOutR, clientOut := io.Pipe()  // we read from the proxy's "stdout"
	childInR, childInW := io.Pipe()     // proxy writes to child stdin
	childOutR, childOutW := io.Pipe()   // child writes to its stdout

	cfg := &policy.Config{Tools: map[string]policy.ToolPolicy{
		"send_email": {Mode: policy.ModeStrict, TTL: time.Hour, FingerprintFields: []string{"to"}},
	}}
	st := store.NewMemory(nil)
	pl := pipeline.New(cfg, st)
	h := NewStdioHandler(pl, "/nonexistent", nil, nil)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var wg sync.WaitGroup
	wg.Add(3)

	// pump client → child
	go func() {
		defer wg.Done()
		defer childInW.Close()
		h.pumpClientToChild(ctx, clientIn, childInW, clientOut)
	}()
	// pump child → client (intercept caching)
	go func() {
		defer wg.Done()
		defer clientOut.Close()
		h.pumpChildToClient(childOutR, clientOut)
	}()
	// fake child process
	go func() {
		defer wg.Done()
		defer childOutW.Close()
		fake.handle(childInR, childOutW)
	}()

	// client speaks
	send := func(id int) {
		frame := map[string]any{
			"jsonrpc": "2.0",
			"id":      id,
			"method":  "tools/call",
			"params":  map[string]any{"name": "send_email", "arguments": map[string]any{"to": "a@b.com"}},
		}
		b, _ := json.Marshal(frame)
		_, _ = clientInW.Write(b)
		_, _ = clientInW.Write([]byte{'\n'})
	}

	send(1)
	resp1 := readLine(t, clientOutR)
	send(2)
	resp2 := readLine(t, clientOutR)

	// give the cache write goroutine a moment then close client input
	time.Sleep(50 * time.Millisecond)
	_ = clientInW.Close()
	_ = childOutW.Close()

	if fake.callCount() != 1 {
		t.Errorf("fake server received %d calls, want 1 (second should be cached)", fake.callCount())
	}

	var m1, m2 Message
	if err := json.Unmarshal(resp1, &m1); err != nil {
		t.Fatalf("decode resp1: %v\nraw: %s", err, resp1)
	}
	if err := json.Unmarshal(resp2, &m2); err != nil {
		t.Fatalf("decode resp2: %v\nraw: %s", err, resp2)
	}
	if !bytes.Equal(m1.Result, m2.Result) {
		t.Errorf("replay result mismatch:\nm1=%s\nm2=%s", m1.Result, m2.Result)
	}
	var id2 int
	_ = json.Unmarshal(m2.ID, &id2)
	if id2 != 2 {
		t.Errorf("replay id = %d, want 2 (must reflect the second client's id)", id2)
	}
}

func readLine(t *testing.T, r io.Reader) []byte {
	t.Helper()
	var buf bytes.Buffer
	one := make([]byte, 1)
	for {
		n, err := r.Read(one)
		if n > 0 {
			if one[0] == '\n' {
				return buf.Bytes()
			}
			buf.WriteByte(one[0])
		}
		if err != nil {
			t.Fatalf("read: %v", err)
		}
	}
}

func TestStdio_NonToolsCallPassesThrough(t *testing.T) {
	fake := &fakeMCPServer{}

	clientIn, clientInW := io.Pipe()
	clientOutR, clientOut := io.Pipe()
	childInR, childInW := io.Pipe()
	childOutR, childOutW := io.Pipe()

	cfg := &policy.Config{Defaults: policy.ToolPolicy{Mode: policy.ModeStrict, TTL: time.Hour}}
	pl := pipeline.New(cfg, store.NewMemory(nil))
	h := NewStdioHandler(pl, "/nonexistent", nil, nil)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var wg sync.WaitGroup
	wg.Add(3)
	go func() { defer wg.Done(); defer childInW.Close(); h.pumpClientToChild(ctx, clientIn, childInW, clientOut) }()
	go func() { defer wg.Done(); defer clientOut.Close(); h.pumpChildToClient(childOutR, clientOut) }()
	go func() { defer wg.Done(); defer childOutW.Close(); fake.handle(childInR, childOutW) }()

	frame := map[string]any{"jsonrpc": "2.0", "id": 1, "method": "tools/list"}
	b, _ := json.Marshal(frame)
	_, _ = clientInW.Write(b)
	_, _ = clientInW.Write([]byte{'\n'})
	_ = readLine(t, clientOutR)
	_ = clientInW.Close()
	_ = childOutW.Close()
}

func TestStdio_CoalescesPipelinedDuplicates(t *testing.T) {
	// Real MCP clients dispatch multiple tools/call frames without waiting
	// for responses. Three identical calls sent before any response should
	// result in exactly one upstream call; the other two should be replayed
	// from the cached leader response with their own correlation ids.

	fake := &fakeMCPServer{}

	clientIn, clientInW := io.Pipe()
	clientOutR, clientOut := io.Pipe()
	childInR, childInW := io.Pipe()
	childOutR, childOutW := io.Pipe()

	cfg := &policy.Config{Tools: map[string]policy.ToolPolicy{
		"send_email": {Mode: policy.ModeStrict, TTL: time.Hour, FingerprintFields: []string{"to"}},
	}}
	pl := pipeline.New(cfg, store.NewMemory(nil))
	h := NewStdioHandler(pl, "/nonexistent", nil, nil)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var wg sync.WaitGroup
	wg.Add(3)
	go func() { defer wg.Done(); defer childInW.Close(); h.pumpClientToChild(ctx, clientIn, childInW, clientOut) }()
	go func() { defer wg.Done(); defer clientOut.Close(); h.pumpChildToClient(childOutR, clientOut) }()
	go func() { defer wg.Done(); defer childOutW.Close(); fake.handle(childInR, childOutW) }()

	// blast 3 identical tools/call frames concurrently with the response
	// reader; io.Pipe has zero buffer so writing and reading must overlap.
	go func() {
		for _, id := range []int{1, 2, 3} {
			frame := map[string]any{
				"jsonrpc": "2.0", "id": id, "method": "tools/call",
				"params": map[string]any{"name": "send_email", "arguments": map[string]any{"to": "a@b.com"}},
			}
			b, _ := json.Marshal(frame)
			b = append(b, '\n')
			_, _ = clientInW.Write(b)
		}
	}()

	// drain 3 responses
	got := make([]Message, 0, 3)
	for i := 0; i < 3; i++ {
		line := readLine(t, clientOutR)
		var m Message
		if err := json.Unmarshal(line, &m); err != nil {
			t.Fatalf("decode #%d: %v\nraw: %s", i, err, line)
		}
		got = append(got, m)
	}

	time.Sleep(50 * time.Millisecond)
	_ = clientInW.Close()
	_ = childOutW.Close()

	if fake.callCount() != 1 {
		t.Errorf("upstream call count = %d, want 1 (the other two should be coalesced)", fake.callCount())
	}

	// all three responses must carry the same result body
	for i := 1; i < len(got); i++ {
		if !bytes.Equal(got[i].Result, got[0].Result) {
			t.Errorf("response %d result differs from response 0:\nr0=%s\nr%d=%s", i, got[0].Result, i, got[i].Result)
		}
	}

	// each response id must be unique and match one of the request ids
	seen := map[int]bool{}
	for _, m := range got {
		var id int
		_ = json.Unmarshal(m.ID, &id)
		if id < 1 || id > 3 {
			t.Errorf("unexpected response id %d", id)
		}
		if seen[id] {
			t.Errorf("duplicate response id %d", id)
		}
		seen[id] = true
	}
	if len(seen) != 3 {
		t.Errorf("expected responses for all 3 request ids, got %v", seen)
	}
}

func TestStdio_ExpireAt_NotifiesWaiters(t *testing.T) {
	// A leader plus two waiters whose deadlines have passed should each
	// receive a JSON-RPC timeout error, and the maps should be cleared.

	cfg := &policy.Config{Tools: map[string]policy.ToolPolicy{
		"send_email": {Mode: policy.ModeStrict, TTL: time.Hour, FingerprintFields: []string{"to"}},
	}}
	pl := pipeline.New(cfg, store.NewMemory(nil))
	h := NewStdioHandler(pl, "/nonexistent", nil, nil)
	h.SetRequestTimeout(50 * time.Millisecond)

	pastDeadline := time.Now().Add(-time.Second)
	h.pending["1"] = pendingCall{tool: "send_email", args: []byte(`{"to":"a"}`), hash: "h1", deadline: pastDeadline}
	h.inflight["h1"] = &inflightCall{waiters: []json.RawMessage{json.RawMessage(`2`), json.RawMessage(`3`)}}

	out := &bytes.Buffer{}
	n := h.expireAt(time.Now(), out)
	if n != 3 {
		t.Errorf("expected 3 timeout responses, got %d", n)
	}
	if len(h.pending) != 0 {
		t.Errorf("pending map not cleared: %v", h.pending)
	}
	if len(h.inflight) != 0 {
		t.Errorf("inflight map not cleared: %v", h.inflight)
	}

	// must be 3 JSON-RPC error lines on the writer
	lines := bytes.Split(bytes.TrimRight(out.Bytes(), "\n"), []byte("\n"))
	if len(lines) != 3 {
		t.Fatalf("expected 3 lines, got %d:\n%s", len(lines), out.String())
	}
	for _, line := range lines {
		var m Message
		if err := json.Unmarshal(line, &m); err != nil {
			t.Errorf("bad timeout frame: %v\nraw: %s", err, line)
			continue
		}
		if m.Error == nil || m.Error.Code != -32000 {
			t.Errorf("expected -32000 error, got %+v", m.Error)
		}
	}
}

func TestStdio_ExpireAt_LeavesUnexpiredAlone(t *testing.T) {
	cfg := &policy.Config{}
	pl := pipeline.New(cfg, store.NewMemory(nil))
	h := NewStdioHandler(pl, "/nonexistent", nil, nil)

	futureDeadline := time.Now().Add(time.Hour)
	h.pending["1"] = pendingCall{tool: "t", args: []byte(`{}`), hash: "h", deadline: futureDeadline}
	h.inflight["h"] = &inflightCall{}

	n := h.expireAt(time.Now(), &bytes.Buffer{})
	if n != 0 {
		t.Errorf("nothing should expire, got %d", n)
	}
	if len(h.pending) != 1 || len(h.inflight) != 1 {
		t.Errorf("unexpired entries were removed: pending=%d inflight=%d", len(h.pending), len(h.inflight))
	}
}

func TestStdio_RunFailsOnBadCommand(t *testing.T) {
	cfg := &policy.Config{}
	pl := pipeline.New(cfg, store.NewMemory(nil))
	h := NewStdioHandler(pl, "/does/not/exist/binary", nil, nil)
	err := h.Run(context.Background(), strings.NewReader(""), io.Discard)
	if err == nil {
		t.Errorf("expected error for nonexistent command")
	}
	_ = (*exec.Cmd)(nil) // silence unused import for older builds
}
