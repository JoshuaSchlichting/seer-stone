package main

import (
	"bufio"
	"encoding/json"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"
)

func TestPiDatabaseReadyStatus(t *testing.T) {
	s := &piAssistantSession{toolReady: make(chan struct{})}
	record := map[string]any{"type": "extension_ui_request", "method": "setStatus", "statusKey": "seer_stone_database", "statusText": "Seer Stone query tool ready"}
	s.handleRecord(record)
	s.handleRecord(record) // Repeated announcements must not panic.
	select {
	case <-s.toolReady:
	default:
		t.Fatal("database tool readiness not recorded")
	}
}

func TestPiDatabaseExecutionEvents(t *testing.T) {
	s := &piAssistantSession{}
	s.handleRecord(map[string]any{"type": "tool_execution_start", "toolName": piDatabaseToolName, "args": map[string]any{"query": "SELECT COUNT(*) FROM auth.user_info"}})
	s.handleRecord(map[string]any{"type": "tool_execution_end", "toolName": piDatabaseToolName, "isError": false, "result": map[string]any{"content": []any{map[string]any{"type": "text", "text": "42"}}}})
	if len(s.events) != 4 || s.events[0].Type != "database_query" || s.events[0].Query == "" || s.events[2].Type != "database_result" || s.events[2].Text != "42" {
		t.Fatalf("missing execution evidence: %+v", s.events)
	}
}

func TestStartPiAssistantRetainsDatabaseBridge(t *testing.T) {
	if os.Getenv("SEER_STONE_TEST_PI") != "1" {
		t.Skip("set SEER_STONE_TEST_PI=1 to test installed Pi")
	}
	service := &DatabaseService{
		profiles:   []ConnectionProfile{{ID: "test", Database: "test", AllowPiDatabaseAccess: true}},
		piSessions: make(map[string]*piAssistantSession),
	}
	id, err := service.StartPiAssistant("Test database context. No credentials or live data.", "test", "test-password", "test", true)
	if err != nil {
		t.Fatal(err)
	}
	defer service.StopPiAssistant(id)
	session := service.piSession(id)
	if session.bridge == nil {
		t.Fatal("startup lost its database bridge")
	}
	args := strings.Join(session.cmd.Args, " ")
	if strings.Contains(args, "--no-tools") || !strings.Contains(args, "--extension ") {
		t.Fatalf("database-enabled launch lacks tool: %s", args)
	}
	prompt, err := os.ReadFile(session.contextPath)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(prompt), "No live database tool is enabled") {
		t.Fatal("database-enabled session has disabled-tool instructions")
	}
}

// This tests the real generated extension against the installed CLI without
// sending a model prompt or accessing any database.
func TestPiDatabaseExtensionRPC(t *testing.T) {
	if os.Getenv("SEER_STONE_TEST_PI") != "1" {
		t.Skip("set SEER_STONE_TEST_PI=1 to test installed Pi")
	}
	pi, err := findPiExecutable()
	if err != nil {
		t.Fatal(err)
	}
	path, err := createPiDatabaseExtension("http://127.0.0.1:1/query", "test-token", false, true)
	if err != nil {
		t.Fatal(err)
	}
	defer os.Remove(path)
	cmd := exec.Command(pi, "--mode", "rpc", "--no-session", "--no-extensions", "--no-skills", "--no-context-files", "--no-mcp", "--no-builtin-tools", "--extension", path)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	defer stdin.Close()
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = cmd.Process.Kill(); _ = cmd.Wait() }()
	ready := make(chan bool, 1)
	go func() {
		scanner := bufio.NewScanner(stdout)
		for scanner.Scan() {
			var record map[string]any
			if json.Unmarshal(scanner.Bytes(), &record) != nil {
				continue
			}
			if record["type"] == "extension_ui_request" && record["statusText"] == "Seer Stone query tool ready" {
				ready <- true
				return
			}
		}
		ready <- false
	}()
	select {
	case ok := <-ready:
		if !ok {
			t.Fatal("Pi exited without activating the database tool")
		}
	case <-time.After(30 * time.Second):
		t.Fatal("Pi did not announce the active database tool")
	}
}
