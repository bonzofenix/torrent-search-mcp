package main

import (
	"bufio"
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

var binary string

func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "torrent-search-mcp")
	if err != nil {
		panic(err)
	}
	binary = filepath.Join(dir, "torrent-search-mcp")
	build := exec.Command("go", "build", "-o", binary, ".")
	build.Stderr = os.Stderr
	if err := build.Run(); err != nil {
		panic(err)
	}
	code := m.Run()
	os.RemoveAll(dir)
	os.Exit(code)
}

type session struct {
	t      *testing.T
	cmd    *exec.Cmd
	stdin  io.WriteCloser
	stdout *bufio.Reader
	stderr *strings.Builder
	id     int
}

func start(t *testing.T, args ...string) *session {
	t.Helper()
	cmd := exec.Command(binary, args...)
	cmd.Env = append(os.Environ(), "WEBUI_URL=")
	stdin, _ := cmd.StdinPipe()
	stdout, _ := cmd.StdoutPipe()
	stderr := &strings.Builder{}
	cmd.Stderr = stderr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { cmd.Process.Kill() })
	s := &session{t: t, cmd: cmd, stdin: stdin, stdout: bufio.NewReader(stdout), stderr: stderr}
	s.call("initialize", map[string]any{"protocolVersion": "2025-06-18", "capabilities": map[string]any{},
		"clientInfo": map[string]any{"name": "test", "version": "1"}})
	s.send(map[string]any{"jsonrpc": "2.0", "method": "notifications/initialized"})
	return s
}

func (s *session) send(msg any) {
	data, _ := json.Marshal(msg)
	if _, err := s.stdin.Write(append(data, '\n')); err != nil {
		s.t.Fatal(err)
	}
}

func (s *session) call(method string, params any) map[string]any {
	s.id++
	s.send(map[string]any{"jsonrpc": "2.0", "id": s.id, "method": method, "params": params})
	line, err := s.stdout.ReadBytes('\n')
	if err != nil {
		s.t.Fatalf("reading %s response: %v (stderr: %s)", method, err, s.stderr)
	}
	var resp map[string]any
	if err := json.Unmarshal(line, &resp); err != nil {
		s.t.Fatalf("stdout must carry JSON-RPC only, got %q", line)
	}
	return resp
}

func (s *session) webapp() string {
	resp := s.call("tools/call", map[string]any{"name": "torrent_webapp", "arguments": map[string]any{}})
	content := resp["result"].(map[string]any)["content"].([]any)
	return content[0].(map[string]any)["text"].(string)
}

func (s *session) wait() int {
	done := make(chan error, 1)
	go func() { done <- s.cmd.Wait() }()
	select {
	case err := <-done:
		var exit *exec.ExitError
		if errors.As(err, &exit) {
			return exit.ExitCode()
		}
		if err != nil {
			s.t.Fatal(err)
		}
		return 0
	case <-time.After(10 * time.Second):
		s.t.Fatal("server did not exit")
		return -1
	}
}

func TestExitsCleanlyWhenStdinCloses(t *testing.T) {
	s := start(t, "--mode", "stdio")
	if !strings.HasPrefix(s.webapp(), "webapp URL not configured") {
		t.Fatal("unexpected tool result")
	}
	s.stdin.Close()
	if code := s.wait(); code != 0 {
		t.Fatalf("exit code = %d", code)
	}
	if !strings.Contains(s.stderr.String(), "Starting MCP server") {
		t.Fatalf("logs belong on stderr, got %q", s.stderr)
	}
}

func TestExitsCleanlyOnSignals(t *testing.T) {
	for _, sig := range []syscall.Signal{syscall.SIGTERM, syscall.SIGINT} {
		s := start(t)
		s.webapp()
		s.cmd.Process.Signal(sig)
		if code := s.wait(); code != 0 {
			t.Fatalf("%v exit code = %d", sig, code)
		}
	}
}

func TestSurvivesHangup(t *testing.T) {
	s := start(t)
	s.cmd.Process.Signal(syscall.SIGHUP)
	time.Sleep(200 * time.Millisecond)
	s.webapp() // still serving
	s.stdin.Close()
	if code := s.wait(); code != 0 {
		t.Fatalf("exit code = %d", code)
	}
}

func TestRejectsOtherModes(t *testing.T) {
	out, err := exec.Command(binary, "--mode", "http").CombinedOutput()
	var exit *exec.ExitError
	if !errors.As(err, &exit) || exit.ExitCode() != 2 || !strings.Contains(string(out), "not supported") {
		t.Fatalf("http mode = %v %s", err, out)
	}
	out, err = exec.Command(binary, "--version").Output()
	if err != nil || strings.TrimSpace(string(out)) != "dev" {
		t.Fatalf("--version = %q %v", out, err)
	}
}
