package mcp

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"os/exec"
	"sync"
	"time"

	"github.com/potent/potent/internal/pipeline"
)

// StdioHandler spawns an MCP server as a child process and proxies JSON-RPC
// frames between the parent's stdin/stdout and the child's stdin/stdout.
// On each client→server tools/call request, the pipeline is applied: a
// cache hit short-circuits the child entirely; a miss forwards as usual
// and the child's response is captured + cached on the way back.
//
// Pipelined duplicate requests (real MCP clients dispatch multiple
// tools/call frames without waiting for responses) are coalesced: the
// first request becomes the "leader" and is forwarded; subsequent
// duplicates wait on the leader's response and receive replays.
//
// Server→client messages (notifications, results to earlier requests,
// server-initiated requests like sampling/createMessage) flow through
// untouched.
type StdioHandler struct {
	pipeline *pipeline.Pipeline
	cmdPath  string
	cmdArgs  []string
	logger   *slog.Logger

	// requestTimeout bounds how long a forwarded tools/call may sit in the
	// pending+inflight maps before being treated as failed. A hung or
	// crashed child server otherwise leaks one map entry per orphaned
	// request indefinitely.
	requestTimeout time.Duration

	mu       sync.Mutex
	pending  map[string]pendingCall   // request id -> leader call
	inflight map[string]*inflightCall // fingerprint hash -> in-flight forward

	// writeMu serializes writes to clientOut. The dedup pipeline writes from
	// both pumps (replays from the client pump, leader and fan-out from the
	// child pump); without serialization their byte streams can interleave
	// on the same writer.
	writeMu sync.Mutex
}

type pendingCall struct {
	tool      string
	args      []byte
	hash      string
	deadline  time.Time
}

// inflightCall coalesces concurrent duplicate tools/call requests. The leader
// is the request that was actually forwarded to the child; waiters are
// subsequent duplicates that arrived before the leader's response was cached.
type inflightCall struct {
	waiters  []json.RawMessage // client request ids awaiting the leader's response
	deadline time.Time
}

// DefaultRequestTimeout is how long an in-flight tools/call may wait for a
// response from the child before being garbage-collected.
const DefaultRequestTimeout = 60 * time.Second

// NewStdioHandler constructs a stdio MCP adapter that will exec the given
// command as the upstream MCP server.
func NewStdioHandler(pl *pipeline.Pipeline, cmdPath string, cmdArgs []string, logger *slog.Logger) *StdioHandler {
	if logger == nil {
		logger = slog.Default()
	}
	return &StdioHandler{
		pipeline:       pl,
		cmdPath:        cmdPath,
		cmdArgs:        cmdArgs,
		logger:         logger,
		requestTimeout: DefaultRequestTimeout,
		pending:        make(map[string]pendingCall),
		inflight:       make(map[string]*inflightCall),
	}
}

// SetRequestTimeout overrides the default in-flight request timeout. Pass 0
// to disable expiry (not recommended in production).
func (h *StdioHandler) SetRequestTimeout(d time.Duration) {
	h.requestTimeout = d
}

// Run starts the child and pumps frames until ctx is cancelled or either
// side closes its pipe. clientIn is the parent's stdin (client→proxy);
// clientOut is the parent's stdout (proxy→client).
func (h *StdioHandler) Run(ctx context.Context, clientIn io.Reader, clientOut io.Writer) error {
	cmd := exec.CommandContext(ctx, h.cmdPath, h.cmdArgs...) // #nosec G204 -- operator-supplied command
	childIn, err := cmd.StdinPipe()
	if err != nil {
		return fmt.Errorf("stdin pipe: %w", err)
	}
	childOut, err := cmd.StdoutPipe()
	if err != nil {
		return fmt.Errorf("stdout pipe: %w", err)
	}
	cmd.Stderr = nil // let the operator wire stderr if they want it
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("start child: %w", err)
	}

	sweepCtx, stopSweep := context.WithCancel(ctx)
	defer stopSweep()

	var wg sync.WaitGroup
	wg.Add(2)

	// client → proxy → (maybe child)
	go func() {
		defer wg.Done()
		defer childIn.Close()
		h.pumpClientToChild(ctx, clientIn, childIn, clientOut)
	}()

	// child → proxy → client (intercept responses to cache)
	go func() {
		defer wg.Done()
		h.pumpChildToClient(childOut, clientOut)
	}()

	// background sweeper expires forgotten requests so a hung or crashed
	// child server doesn't leak map entries forever.
	if h.requestTimeout > 0 {
		go h.sweepExpired(sweepCtx, clientOut)
	}

	wg.Wait()
	stopSweep()
	return cmd.Wait()
}

func (h *StdioHandler) pumpClientToChild(ctx context.Context, in io.Reader, childIn io.Writer, clientOut io.Writer) {
	sc := bufio.NewScanner(in)
	sc.Buffer(make([]byte, 64*1024), 4*1024*1024)
	for sc.Scan() {
		line := sc.Bytes()
		copyLine := append([]byte(nil), line...)

		var msg Message
		if err := json.Unmarshal(copyLine, &msg); err != nil {
			h.writeLine(childIn, copyLine)
			continue
		}

		if msg.Method != MethodToolsCall {
			h.writeLine(childIn, copyLine)
			continue
		}

		tool, args, err := ParseToolsCall(&msg)
		if err != nil {
			h.writeLine(childIn, copyLine)
			continue
		}

		// mcp-stdio is single-tenant (the client shares this process), so
		// no caller-id is plumbed through; ACLs are an http-only feature.
		res, _, err := h.pipeline.Lookup(ctx, tool, "", args)
		if err != nil {
			h.logger.Warn("pipeline lookup", "err", err)
			h.writeLine(childIn, copyLine)
			continue
		}

		switch res.Decision {
		case pipeline.DecisionReplay:
			h.logger.Info("stdio replay", "tool", tool, "match", res.Match.Kind, "similarity", res.Match.Similarity)
			h.writeClient(clientOut, reframeWithID(res.Body, msg.ID))
		case pipeline.DecisionBlock:
			h.logger.Info("stdio block", "tool", tool)
			blocked := NewErrorResponse(msg.ID, -32000, "duplicate request blocked by policy")
			out, _ := json.Marshal(blocked)
			h.writeClient(clientOut, out)
		default:
			deadline := time.Time{}
			if h.requestTimeout > 0 {
				deadline = time.Now().Add(h.requestTimeout)
			}
			h.mu.Lock()
			if ifc, ok := h.inflight[res.Hash]; ok {
				// a sibling forward is already in flight; queue as a waiter
				// so we get a replay when the leader's response lands.
				idCopy := append(json.RawMessage(nil), msg.ID...)
				ifc.waiters = append(ifc.waiters, idCopy)
				h.mu.Unlock()
				h.logger.Info("stdio coalesce", "tool", tool, "hash", res.Hash)
				continue
			}
			h.pending[string(msg.ID)] = pendingCall{tool: tool, args: args, hash: res.Hash, deadline: deadline}
			h.inflight[res.Hash] = &inflightCall{deadline: deadline}
			h.mu.Unlock()
			h.writeLine(childIn, copyLine)
		}
	}
}

func (h *StdioHandler) pumpChildToClient(childOut io.Reader, clientOut io.Writer) {
	sc := bufio.NewScanner(childOut)
	sc.Buffer(make([]byte, 64*1024), 4*1024*1024)
	for sc.Scan() {
		line := sc.Bytes()
		copyLine := append([]byte(nil), line...)

		var msg Message
		if err := json.Unmarshal(copyLine, &msg); err == nil && msg.IsResponse() {
			waiters := h.maybeCacheResponse(&msg, copyLine)
			h.writeClient(clientOut, copyLine)
			// fan out the cached body to coalesced duplicate requests
			for _, waiterID := range waiters {
				h.writeClient(clientOut, reframeWithID(copyLine, waiterID))
			}
			continue
		}
		h.writeClient(clientOut, copyLine)
	}
}

// maybeCacheResponse stores the response and returns the list of pipelined
// duplicate request ids that were waiting on this leader's result. Each
// waiter id should receive the same response body (with its own id substituted).
func (h *StdioHandler) maybeCacheResponse(msg *Message, raw []byte) []json.RawMessage {
	h.mu.Lock()
	call, ok := h.pending[string(msg.ID)]
	if !ok {
		h.mu.Unlock()
		return nil
	}
	delete(h.pending, string(msg.ID))
	h.mu.Unlock()

	// cache outside the lock; Cache may touch disk / network for embeddings
	if err := h.pipeline.Cache(context.Background(), call.tool, call.args, 200, raw); err != nil {
		h.logger.Warn("cache response", "tool", call.tool, "err", err)
	}

	// now collect waiters and clear the in-flight entry. Any tools/call that
	// arrived between the Cache write and this delete will still see inflight
	// and be appended to waiters; that's correct behavior.
	h.mu.Lock()
	ifc := h.inflight[call.hash]
	delete(h.inflight, call.hash)
	h.mu.Unlock()
	if ifc == nil {
		return nil
	}
	if len(ifc.waiters) > 0 {
		h.logger.Info("stdio fan-out", "tool", call.tool, "waiters", len(ifc.waiters))
	}
	return ifc.waiters
}

// minSweepInterval bounds how often the sweeper can run regardless of
// requestTimeout. Operators with very short timeouts (e.g. test or local
// debugging) still get reasonably prompt expiry; production deployments
// with the 60s default get a ~15s tick which is plenty.
var minSweepInterval = 500 * time.Millisecond

// sweepExpired periodically scans the pending and inflight maps for entries
// past their deadline, releases them, and notifies any waiting clients
// with a JSON-RPC error so they don't hang forever.
func (h *StdioHandler) sweepExpired(ctx context.Context, clientOut io.Writer) {
	interval := h.requestTimeout / 4
	if interval < minSweepInterval {
		interval = minSweepInterval
	}
	tick := time.NewTicker(interval)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case now := <-tick.C:
			h.expireAt(now, clientOut)
		}
	}
}

// expireAt removes entries with deadlines in the past, emits timeout errors
// to waiters, and returns the number of expirations (useful for metrics).
func (h *StdioHandler) expireAt(now time.Time, clientOut io.Writer) int {
	type expired struct {
		ids []json.RawMessage // leader id + waiter ids that need a timeout error
	}
	var toNotify []expired

	h.mu.Lock()
	for id, call := range h.pending {
		if !call.deadline.IsZero() && now.After(call.deadline) {
			ids := []json.RawMessage{json.RawMessage(id)}
			if ifc, ok := h.inflight[call.hash]; ok {
				ids = append(ids, ifc.waiters...)
				delete(h.inflight, call.hash)
			}
			delete(h.pending, id)
			toNotify = append(toNotify, expired{ids: ids})
		}
	}
	h.mu.Unlock()

	count := 0
	for _, e := range toNotify {
		for _, id := range e.ids {
			resp := NewErrorResponse(id, -32000, "upstream timeout")
			out, _ := json.Marshal(resp)
			h.writeClient(clientOut, out)
			count++
		}
	}
	if count > 0 {
		h.logger.Warn("stdio sweep expired", "responses_sent", count)
	}
	return count
}

// writeLine writes a single JSON-RPC frame plus terminating newline as one
// atomic Write so concurrent callers don't interleave bytes on the writer.
// When the writer is the shared clientOut, writeMu must be held to keep
// successive frames from being mixed by the receiver.
func (h *StdioHandler) writeLine(w io.Writer, line []byte) {
	var buf []byte
	if len(line) > 0 && line[len(line)-1] == '\n' {
		buf = line
	} else {
		buf = make([]byte, 0, len(line)+1)
		buf = append(buf, line...)
		buf = append(buf, '\n')
	}
	_, _ = w.Write(buf)
}

// writeClient is the only entry point that should write to the shared
// clientOut. It serializes concurrent writers so frames don't interleave.
func (h *StdioHandler) writeClient(clientOut io.Writer, line []byte) {
	h.writeMu.Lock()
	h.writeLine(clientOut, line)
	h.writeMu.Unlock()
}
