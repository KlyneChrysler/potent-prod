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

	"github.com/potent/potent/internal/pipeline"
)

// StdioHandler spawns an MCP server as a child process and proxies JSON-RPC
// frames between the parent's stdin/stdout and the child's stdin/stdout.
// On each client→server tools/call request, the pipeline is applied: a
// cache hit short-circuits the child entirely; a miss forwards as usual
// and the child's response is captured + cached on the way back.
//
// Server→client messages (notifications, results to earlier requests,
// server-initiated requests like sampling/createMessage) flow through
// untouched.
type StdioHandler struct {
	pipeline *pipeline.Pipeline
	cmdPath  string
	cmdArgs  []string
	logger   *slog.Logger

	// pending tracks the tool name and original client id for in-flight
	// tools/call requests so the response from the child can be matched
	// back and cached against the right tool.
	mu      sync.Mutex
	pending map[string]pendingCall
}

type pendingCall struct {
	tool string
	args []byte
}

// NewStdioHandler constructs a stdio MCP adapter that will exec the given
// command as the upstream MCP server.
func NewStdioHandler(pl *pipeline.Pipeline, cmdPath string, cmdArgs []string, logger *slog.Logger) *StdioHandler {
	if logger == nil {
		logger = slog.Default()
	}
	return &StdioHandler{
		pipeline: pl,
		cmdPath:  cmdPath,
		cmdArgs:  cmdArgs,
		logger:   logger,
		pending:  make(map[string]pendingCall),
	}
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

	wg.Wait()
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

		res, _, err := h.pipeline.Lookup(ctx, tool, args)
		if err != nil {
			h.logger.Warn("pipeline lookup", "err", err)
			h.writeLine(childIn, copyLine)
			continue
		}

		switch res.Decision {
		case pipeline.DecisionReplay:
			h.logger.Info("stdio replay", "tool", tool, "match", res.Match.Kind, "similarity", res.Match.Similarity)
			h.writeLine(clientOut, reframeWithID(res.Body, msg.ID))
		case pipeline.DecisionBlock:
			h.logger.Info("stdio block", "tool", tool)
			blocked := NewErrorResponse(msg.ID, -32000, "duplicate request blocked by policy")
			out, _ := json.Marshal(blocked)
			h.writeLine(clientOut, out)
		default:
			h.mu.Lock()
			h.pending[string(msg.ID)] = pendingCall{tool: tool, args: args}
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
			h.maybeCacheResponse(&msg, copyLine)
		}
		h.writeLine(clientOut, copyLine)
	}
}

func (h *StdioHandler) maybeCacheResponse(msg *Message, raw []byte) {
	h.mu.Lock()
	call, ok := h.pending[string(msg.ID)]
	if ok {
		delete(h.pending, string(msg.ID))
	}
	h.mu.Unlock()
	if !ok {
		return
	}
	if err := h.pipeline.Cache(context.Background(), call.tool, call.args, 200, raw); err != nil {
		h.logger.Warn("cache response", "tool", call.tool, "err", err)
	}
}

func (h *StdioHandler) writeLine(w io.Writer, line []byte) {
	if _, err := w.Write(line); err != nil {
		return
	}
	if len(line) == 0 || line[len(line)-1] != '\n' {
		_, _ = w.Write([]byte{'\n'})
	}
}
