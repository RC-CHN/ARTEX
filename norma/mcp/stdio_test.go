package mcp

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestStdioHelper(t *testing.T) {
	if os.Getenv("ARTEX_MCP_TEST_HELPER") != "1" {
		return
	}
	if file := os.Getenv("ARTEX_MCP_CHILD_PID"); file != "" {
		c := exec.Command("sleep", "60")
		if e := c.Start(); e != nil {
			os.Exit(3)
		}
		_ = os.WriteFile(file, []byte(strconv.Itoa(c.Process.Pid)), 0600)
	}
	scanner := bufio.NewScanner(os.Stdin)
	enc := json.NewEncoder(os.Stdout)
	for scanner.Scan() {
		var request map[string]any
		_ = json.Unmarshal(scanner.Bytes(), &request)
		id, ok := request["id"]
		if !ok {
			continue
		}
		var result any
		switch request["method"] {
		case "initialize":
			result = map[string]any{"protocolVersion": "2024-11-05"}
		case "tools/list":
			result = map[string]any{"tools": []any{map[string]any{"name": "inspect", "inputSchema": map[string]any{"type": "object"}}}}
		case "tools/call":
			cwd, _ := os.Getwd()
			result = map[string]any{"content": []any{map[string]any{"type": "text", "text": fmt.Sprintf("cwd=%s isolated=%s secret=%s", cwd, os.Getenv("ISOLATED"), os.Getenv("CONTROLLER_SECRET"))}}}
		}
		_ = enc.Encode(map[string]any{"jsonrpc": "2.0", "id": id, "result": result})
	}
	os.Exit(0)
}
func TestStdioExplicitWorkingDirAndEnvironment(t *testing.T) {
	t.Setenv("CONTROLLER_SECRET", "must-not-inherit")
	dir := t.TempDir()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	c, e := NewStdioClientWithOptions(ctx, "test", os.Args[0], StdioOptions{WorkingDir: dir, Env: []string{"ARTEX_MCP_TEST_HELPER=1", "ISOLATED=yes"}}, "-test.run=^TestStdioHelper$")
	if e != nil {
		t.Fatal(e)
	}
	defer c.Close()
	tools, e := c.Tools(ctx)
	if e != nil {
		t.Fatal(e)
	}
	r, e := tools[0].Call(ctx, json.RawMessage(`{}`), nil)
	if e != nil {
		t.Fatal(e)
	}
	if !strings.Contains(r.Flatten(), "cwd="+dir) || !strings.Contains(r.Flatten(), "isolated=yes") || strings.Contains(r.Flatten(), "must-not-inherit") {
		t.Fatalf("unexpected subprocess environment: %s", r.Flatten())
	}
}
func TestStdioCloseKillsChildProcessGroup(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("Linux process group contract")
	}
	file := filepath.Join(t.TempDir(), "pid")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	c, e := NewStdioClientWithOptions(ctx, "test", os.Args[0], StdioOptions{Env: []string{"ARTEX_MCP_TEST_HELPER=1", "ARTEX_MCP_CHILD_PID=" + file, "PATH=/usr/bin:/bin"}}, "-test.run=^TestStdioHelper$")
	if e != nil {
		t.Fatal(e)
	}
	b, e := os.ReadFile(file)
	if e != nil {
		t.Fatal(e)
	}
	if e = c.Close(); e != nil {
		t.Fatal(e)
	}
	_ = c.Close()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		stat, e := os.ReadFile("/proc/" + string(b) + "/stat")
		if os.IsNotExist(e) || strings.Contains(string(stat), ") Z ") {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("MCP child survived closing the client")
}

func TestStdioResolvesExplicitPATH(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Unix executable naming")
	}
	dir := t.TempDir()
	if e := os.Symlink(os.Args[0], filepath.Join(dir, "configured-mcp")); e != nil {
		t.Fatal(e)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	c, e := NewStdioClientWithOptions(ctx, "configured", "configured-mcp", StdioOptions{Env: []string{"PATH=" + dir, "ARTEX_MCP_TEST_HELPER=1"}}, "-test.run=^TestStdioHelper$")
	if e != nil {
		t.Fatal(e)
	}
	c.Close()
}
