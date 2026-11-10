package app

import (
	"bufio"
	"context"
	"encoding/json"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestMCPHelper(t *testing.T) {
	if os.Getenv("MAILROOM_MCP_HELPER") != "1" {
		return
	}
	if e := RunMCP(context.Background(), ConfigFromEnv()); e != nil {
		os.Exit(2)
	}
	os.Exit(0)
}
func TestMCPRealStdio(t *testing.T) {
	a := testApp(t)
	l, e := net.Listen("tcp", "127.0.0.1:0")
	if e != nil {
		t.Fatal(e)
	}
	a.C.Addr = l.Addr().String()
	srv := &http.Server{Handler: a.Handler(context.Background())}
	go srv.Serve(l)
	defer srv.Close()
	if e = os.WriteFile(filepath.Join(a.C.DataDir, "mcp-token"), []byte(a.AgentToken), 0600); e != nil {
		t.Fatal(e)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	cmd := exec.Command(os.Args[0], "-test.run=TestMCPHelper")
	cmd.Env = append(os.Environ(), "MAILROOM_MCP_HELPER=1", "MAILROOM_DATA_DIR="+a.C.DataDir, "MAILROOM_URL=http://"+a.C.Addr)
	client := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "1"}, nil)
	session, e := client.Connect(ctx, &mcp.CommandTransport{Command: cmd}, nil)
	if e != nil {
		t.Fatal(e)
	}
	defer session.Close()
	list, e := session.ListTools(ctx, &mcp.ListToolsParams{})
	if e != nil || len(list.Tools) != 4 {
		t.Fatal(list, e)
	}
	r, e := session.CallTool(ctx, &mcp.CallToolParams{Name: "list_pending", Arguments: map[string]any{}})
	if e != nil || r.IsError {
		t.Fatal(r, e)
	}
	r, e = session.CallTool(ctx, &mcp.CallToolParams{Name: "get_email", Arguments: map[string]any{"id": "demo-4"}})
	if e != nil || r.IsError {
		t.Fatal(r, e)
	}
	r, e = session.CallTool(ctx, &mcp.CallToolParams{Name: "apply_action", Arguments: map[string]any{"id": "demo-1", "action": "trash"}})
	if e != nil || !r.IsError {
		t.Fatal("agent trash must be denied", r, e)
	}
	r, e = session.CallTool(ctx, &mcp.CallToolParams{Name: "complete_review", Arguments: map[string]any{"id": "demo-4", "verdict": "keep", "reason": "实际计划变更"}})
	if e != nil || r.IsError {
		t.Fatal(r, e)
	}
	m, _ := a.Store.Get("demo-4")
	if m.State != "keep" {
		t.Fatal(m.State)
	}
}
func TestCodexHelper(t *testing.T) {
	if os.Getenv("MAILROOM_CODEX_HELPER") != "1" {
		return
	}
	scan := bufio.NewScanner(os.Stdin)
	enc := json.NewEncoder(os.Stdout)
	for scan.Scan() {
		var m map[string]any
		_ = json.Unmarshal(scan.Bytes(), &m)
		switch m["method"] {
		case "initialize":
			_ = enc.Encode(map[string]any{"id": m["id"], "result": map[string]any{}})
		case "thread/start", "thread/resume":
			_ = enc.Encode(map[string]any{"id": m["id"], "result": map[string]any{"thread": map[string]string{"id": "test-thread"}}})
		case "turn/start":
			p := m["params"].(map[string]any)
			input := p["input"].([]any)[0].(map[string]any)["text"].(string)
			if !strings.Contains(input, "Do not archive, trash or restore") {
				os.Exit(3)
			}
			_ = enc.Encode(map[string]any{"id": m["id"], "result": map[string]any{}})
			_ = enc.Encode(map[string]any{"method": "turn/completed", "params": map[string]any{"turn": map[string]string{"status": "completed"}}})
			os.Exit(0)
		}
	}
	os.Exit(2)
}
func TestCodexAppServerBridge(t *testing.T) {
	a := testApp(t)
	a.C.Demo = false
	t.Setenv("MAILROOM_CODEX_HELPER", "1")
	script := filepath.Join(t.TempDir(), "codex-stub")
	body := "#!/bin/sh\nexec " + strconv.Quote(os.Args[0]) + " -test.run=TestCodexHelper -- \"$@\"\n"
	if e := os.WriteFile(script, []byte(body), 0700); e != nil {
		t.Fatal(e)
	}
	a.C.CodexBinary = script
	if e := a.Dispatch(context.Background()); e != nil {
		t.Fatal(e)
	}
	if a.Store.Meta("codex_thread") != "test-thread" {
		t.Fatal("thread was not persisted")
	}
	events, e := a.Store.Events()
	if e != nil || len(events) < 2 || events[0].Action != "codex_completed" {
		t.Fatal(events, e)
	}
	if a.Dispatch(context.Background()) == nil {
		t.Fatal("must throttle duplicate wakeups")
	}
}
func TestDemoLiveIsolation(t *testing.T) {
	dir := t.TempDir()
	a, e := New(Config{Addr: "127.0.0.1:8787", DataDir: dir, Demo: true})
	if e != nil {
		t.Fatal(e)
	}
	a.Store.Close()
	if b, e := New(Config{Addr: "127.0.0.1:8787", DataDir: dir}); e == nil {
		b.Store.Close()
		t.Fatal("demo/live state must not mix")
	}
}
func TestAgentIncompleteReview(t *testing.T) {
	a := testApp(t)
	m, _ := a.Store.Get("demo-4")
	m.Truncated = true
	_ = a.Store.Put(m)
	if e := a.review(context.Background(), m.ID, "promotion", "广告", true); e == nil {
		t.Fatal("incomplete content cannot be downgraded by agent")
	}
}
