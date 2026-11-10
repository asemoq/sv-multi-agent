package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"sync"
	"time"
)

const defaultProtocolVersion = "2024-11-05"

// MCPManager owns the lifecycle of a stdio MCP server and proxies JSON-RPC calls.
//
// The sv-multi-agent service is the only component allowed to launch MCP servers. Workers
// never start processes; they reach the server through the HTTP MCP proxy.
type MCPManager struct {
	Command    []string
	InitParams map[string]string

	mu      sync.Mutex
	cmd     *exec.Cmd
	stdin   io.WriteCloser
	stdout  io.ReadCloser
	stderr  io.ReadCloser
	decoder *json.Decoder
	ctx     context.Context
	cancel  context.CancelFunc
	started bool
}

// NewMCPManager creates a new MCP manager. An empty command disables MCP.
func NewMCPManager(command []string, initParams map[string]string) *MCPManager {
	return &MCPManager{
		Command:    command,
		InitParams: initParams,
	}
}

// Enabled reports whether an MCP server command was configured.
func (m *MCPManager) Enabled() bool {
	return len(m.Command) > 0
}

// Start launches the MCP server process and performs the JSON-RPC handshake.
// It is a no-op when MCP is not configured.
func (m *MCPManager) Start(ctx context.Context) error {
	if !m.Enabled() {
		return nil
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	m.ctx, m.cancel = context.WithCancel(ctx)

	m.cmd = exec.CommandContext(m.ctx, m.Command[0], m.Command[1:]...)
	stdinPipe, err := m.cmd.StdinPipe()
	if err != nil {
		return fmt.Errorf("stdin pipe: %w", err)
	}
	m.stdin = stdinPipe

	stdoutPipe, err := m.cmd.StdoutPipe()
	if err != nil {
		return fmt.Errorf("stdout pipe: %w", err)
	}
	m.stdout = stdoutPipe
	m.decoder = json.NewDecoder(stdoutPipe)

	stderrPipe, err := m.cmd.StderrPipe()
	if err != nil {
		return fmt.Errorf("stderr pipe: %w", err)
	}
	m.stderr = stderrPipe
	// Drain stderr so a chatty MCP server cannot block on a full pipe.
	go func() {
		_, _ = io.Copy(io.Discard, stderrPipe)
	}()

	if err := m.cmd.Start(); err != nil {
		return fmt.Errorf("start process: %w", err)
	}

	// Build a spec-compliant initialize request, allowing overrides.
	params := map[string]any{
		"protocolVersion": defaultProtocolVersion,
		"capabilities":    map[string]any{},
		"clientInfo": map[string]any{
			"name":    "sv-multi-agent",
			"version": "0.2.0",
		},
	}
	for key, value := range m.InitParams {
		params[key] = value
	}

	initReq := map[string]any{
		"jsonrpc": "2.0",
		"id":      1,
		"method":  "initialize",
		"params":  params,
	}
	initResp, err := m.sendRequestLocked(initReq)
	if err != nil {
		return fmt.Errorf("initialize: %w", err)
	}
	if _, ok := initResp["result"]; !ok {
		return fmt.Errorf("initialize failed: %v", initResp)
	}

	if err := m.sendNotificationLocked(map[string]any{
		"jsonrpc": "2.0",
		"method":  "notifications/initialized",
	}); err != nil {
		return fmt.Errorf("send initialized notification: %w", err)
	}

	m.started = true
	return nil
}

// Stop terminates the MCP server process.
func (m *MCPManager) Stop() error {
	m.mu.Lock()
	defer m.mu.Unlock()

	if m.cancel != nil {
		m.cancel()
	}
	if m.stdin != nil {
		_ = m.stdin.Close()
	}
	if m.cmd != nil && m.cmd.Process != nil {
		// Wait returns "signal: killed" after context cancellation; it is expected.
		return m.cmd.Wait()
	}
	return nil
}

// ListTools calls tools/list on the MCP server.
func (m *MCPManager) ListTools(ctx context.Context) ([]map[string]any, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	if err := m.ensureReadyLocked(); err != nil {
		return nil, err
	}

	resp, err := m.sendRequestLocked(map[string]any{
		"jsonrpc": "2.0",
		"id":      time.Now().UnixNano(),
		"method":  "tools/list",
	})
	if err != nil {
		return nil, fmt.Errorf("tools/list: %w", err)
	}

	result, ok := resp["result"]
	if !ok {
		return nil, fmt.Errorf("no result in response: %v", resp)
	}
	resultMap, ok := result.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("result is not an object")
	}
	rawTools, ok := resultMap["tools"].([]any)
	if !ok {
		return nil, fmt.Errorf("result.tools is not an array")
	}

	tools := make([]map[string]any, 0, len(rawTools))
	for i, raw := range rawTools {
		tool, ok := raw.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("tool[%d] is not an object", i)
		}
		tools = append(tools, tool)
	}
	return tools, nil
}

// CallTool calls tools/call on the MCP server.
func (m *MCPManager) CallTool(ctx context.Context, name string, args map[string]any) (map[string]any, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	if err := m.ensureReadyLocked(); err != nil {
		return nil, err
	}
	if name == "" {
		return nil, errors.New("tool name is required")
	}
	if args == nil {
		args = map[string]any{}
	}

	resp, err := m.sendRequestLocked(map[string]any{
		"jsonrpc": "2.0",
		"id":      time.Now().UnixNano(),
		"method":  "tools/call",
		"params": map[string]any{
			"name":      name,
			"arguments": args,
		},
	})
	if err != nil {
		return nil, fmt.Errorf("tools/call: %w", err)
	}

	result, ok := resp["result"]
	if !ok {
		return nil, fmt.Errorf("no result in response: %v", resp)
	}
	if resultMap, ok := result.(map[string]any); ok {
		return resultMap, nil
	}
	return map[string]any{"content": result}, nil
}

func (m *MCPManager) ensureReadyLocked() error {
	if !m.Enabled() {
		return errors.New("MCP is not configured (set MCP_COMMAND)")
	}
	if !m.started {
		return errors.New("MCP server is not running")
	}
	return nil
}

// sendRequestLocked writes a JSON-RPC request and reads one JSON-RPC response.
// The caller must hold m.mu.
func (m *MCPManager) sendRequestLocked(request map[string]any) (map[string]any, error) {
	reqBytes, err := json.Marshal(request)
	if err != nil {
		return nil, fmt.Errorf("marshal request: %w", err)
	}
	reqBytes = append(reqBytes, '\n')

	if _, err := m.stdin.Write(reqBytes); err != nil {
		return nil, fmt.Errorf("write stdin: %w", err)
	}

	var response map[string]any
	if err := m.decoder.Decode(&response); err != nil {
		return nil, fmt.Errorf("decode response: %w", err)
	}
	return response, nil
}

// sendNotificationLocked writes a JSON-RPC notification (no response expected).
// The caller must hold m.mu.
func (m *MCPManager) sendNotificationLocked(notification map[string]any) error {
	reqBytes, err := json.Marshal(notification)
	if err != nil {
		return fmt.Errorf("marshal notification: %w", err)
	}
	reqBytes = append(reqBytes, '\n')
	if _, err := m.stdin.Write(reqBytes); err != nil {
		return fmt.Errorf("write stdin: %w", err)
	}
	return nil
}
