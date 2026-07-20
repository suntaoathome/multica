// daemon-test-agent is a deterministic, credential-free Codex app-server
// fixture for process-level daemon lifecycle tests. It is not shipped in
// production binaries.
package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
)

type request struct {
	ID     json.RawMessage `json:"id"`
	Method string          `json:"method"`
}

func emit(v any) error { return json.NewEncoder(os.Stdout).Encode(v) }

func response(id json.RawMessage, result any) map[string]any {
	var decoded any
	_ = json.Unmarshal(id, &decoded)
	return map[string]any{"jsonrpc": "2.0", "id": decoded, "result": result}
}

func main() {
	if len(os.Args) == 2 && (os.Args[1] == "--version" || os.Args[1] == "version") {
		fmt.Println("codex-cli 0.118.0")
		return
	}
	if len(os.Args) < 2 || os.Args[1] != "app-server" {
		fmt.Fprintln(os.Stderr, "daemon-test-agent: expected app-server")
		os.Exit(2)
	}

	mode := strings.TrimSpace(os.Getenv("MULTICA_TEST_AGENT_MODE"))
	for _, arg := range os.Args[2:] {
		if path, ok := strings.CutPrefix(arg, "--multica-test-control="); ok {
			if data, err := os.ReadFile(filepath.Clean(path)); err == nil {
				mode = strings.TrimSpace(string(data))
			}
		}
	}
	if mode == "" {
		mode = "complete"
	}
	scanner := bufio.NewScanner(os.Stdin)
	for scanner.Scan() {
		var req request
		if err := json.Unmarshal(scanner.Bytes(), &req); err != nil || req.Method == "" {
			continue
		}
		switch req.Method {
		case "initialize", "thread/name/set":
			_ = emit(response(req.ID, map[string]any{}))
		case "thread/start", "thread/resume":
			_ = emit(response(req.ID, map[string]any{"thread": map[string]any{"id": "thr-daemon-e2e"}}))
		case "turn/start":
			_ = emit(response(req.ID, map[string]any{"turn": map[string]any{"id": "turn-daemon-e2e"}}))
			_ = emit(map[string]any{"jsonrpc": "2.0", "method": "turn/started", "params": map[string]any{"threadId": "thr-daemon-e2e", "turn": map[string]any{"id": "turn-daemon-e2e"}}})
			if mode == "block" {
				ch := make(chan os.Signal, 1)
				signal.Notify(ch, syscall.SIGINT, syscall.SIGTERM)
				<-ch
				return
			}
			_ = emit(map[string]any{"jsonrpc": "2.0", "method": "item/completed", "params": map[string]any{"threadId": "thr-daemon-e2e", "item": map[string]any{"id": "msg-daemon-e2e", "type": "agentMessage", "text": "daemon lifecycle fixture completed"}}})
			_ = emit(map[string]any{"jsonrpc": "2.0", "method": "turn/completed", "params": map[string]any{"threadId": "thr-daemon-e2e", "turn": map[string]any{"id": "turn-daemon-e2e", "status": "completed", "usage": map[string]any{"input_tokens": 1, "output_tokens": 1}}}})
		default:
			_ = emit(response(req.ID, map[string]any{}))
		}
	}
	if err := scanner.Err(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
