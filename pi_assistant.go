package main

import (
	"bufio"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
)

type PiAssistantEvent struct {
	Type  string `json:"type"`
	Text  string `json:"text,omitempty"`
	Error string `json:"error,omitempty"`
}

type piAssistantSession struct {
	id          string
	cmd         *exec.Cmd
	stdin       io.WriteCloser
	contextPath string
	writeMu     sync.Mutex
	eventsMu    sync.Mutex
	events      []PiAssistantEvent
}

// StartPiAssistant launches Pi in RPC mode for the in-app SQL assistant panel.
// The provided context must not contain credentials or SQL editor contents.
func (s *DatabaseService) StartPiAssistant(context string) (string, error) {
	if strings.TrimSpace(context) == "" {
		return "", fmt.Errorf("database context is empty")
	}
	if len(context) > 2*1024*1024 {
		return "", fmt.Errorf("SQL assistant context is too large")
	}
	piPath, err := findPiExecutable()
	if err != nil {
		return "", err
	}

	contextFile, err := os.CreateTemp("", "seer-stone-pi-context-*.md")
	if err != nil {
		return "", fmt.Errorf("create Pi context file: %w", err)
	}
	contextPath := contextFile.Name()
	if _, err := contextFile.WriteString(context); err != nil {
		contextFile.Close()
		os.Remove(contextPath)
		return "", fmt.Errorf("write Pi context file: %w", err)
	}
	if err := contextFile.Close(); err != nil {
		os.Remove(contextPath)
		return "", fmt.Errorf("close Pi context file: %w", err)
	}

	cmd := exec.Command(piPath,
		"--mode", "rpc",
		"--no-session",
		"--no-tools",
		"--no-extensions",
		"--no-skills",
		"--no-context-files",
		"--append-system-prompt", contextPath,
	)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		os.Remove(contextPath)
		return "", fmt.Errorf("start Pi RPC input: %w", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		stdin.Close()
		os.Remove(contextPath)
		return "", fmt.Errorf("start Pi RPC output: %w", err)
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		stdin.Close()
		os.Remove(contextPath)
		return "", fmt.Errorf("start Pi diagnostics: %w", err)
	}
	if err := cmd.Start(); err != nil {
		stdin.Close()
		os.Remove(contextPath)
		return "", fmt.Errorf("start Pi: %w", err)
	}

	var idBytes [12]byte
	if _, err := rand.Read(idBytes[:]); err != nil {
		cmd.Process.Kill()
		os.Remove(contextPath)
		return "", fmt.Errorf("create Pi session ID: %w", err)
	}
	sessionID := hex.EncodeToString(idBytes[:])
	session := &piAssistantSession{id: sessionID, cmd: cmd, stdin: stdin, contextPath: contextPath}
	s.piMu.Lock()
	s.piSessions[sessionID] = session
	s.piMu.Unlock()
	go session.readOutput(stdout, stderr)
	return sessionID, nil
}

// SendPiAssistantPrompt submits a user message to an existing in-app Pi session.
func (s *DatabaseService) SendPiAssistantPrompt(sessionID, message string) error {
	if strings.TrimSpace(message) == "" {
		return fmt.Errorf("enter a message for Pi")
	}
	session := s.piSession(sessionID)
	if session == nil {
		return fmt.Errorf("Pi assistant session is no longer available")
	}
	return session.writeRecord(map[string]any{"type": "prompt", "message": message})
}

// StopPiAssistant closes one in-app Pi RPC session.
func (s *DatabaseService) StopPiAssistant(sessionID string) error {
	s.piMu.Lock()
	session := s.piSessions[sessionID]
	delete(s.piSessions, sessionID)
	s.piMu.Unlock()
	if session == nil {
		return nil
	}
	session.writeMu.Lock()
	defer session.writeMu.Unlock()
	_ = session.stdin.Close()
	if session.cmd.Process != nil {
		_ = session.cmd.Process.Kill()
	}
	os.Remove(session.contextPath)
	return nil
}

// PiAssistantEvents drains events accumulated by the Pi RPC subprocess.
func (s *DatabaseService) PiAssistantEvents(sessionID string) ([]PiAssistantEvent, error) {
	session := s.piSession(sessionID)
	if session == nil {
		return nil, fmt.Errorf("Pi assistant session is no longer available")
	}
	session.eventsMu.Lock()
	defer session.eventsMu.Unlock()
	events := append([]PiAssistantEvent(nil), session.events...)
	session.events = session.events[:0]
	return events, nil
}

func (s *DatabaseService) piSession(id string) *piAssistantSession {
	s.piMu.Lock()
	defer s.piMu.Unlock()
	return s.piSessions[id]
}

func (s *piAssistantSession) writeRecord(record any) error {
	data, err := json.Marshal(record)
	if err != nil {
		return err
	}
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	if _, err := s.stdin.Write(append(data, '\n')); err != nil {
		return fmt.Errorf("send message to Pi: %w", err)
	}
	return nil
}

func (s *piAssistantSession) readOutput(stdout, stderr io.ReadCloser) {
	stderrDone := make(chan string, 1)
	go func() {
		var diagnostics strings.Builder
		scanner := bufio.NewScanner(stderr)
		for scanner.Scan() {
			if diagnostics.Len() < 16*1024 {
				diagnostics.WriteString(scanner.Text())
				diagnostics.WriteByte('\n')
			}
		}
		stderrDone <- strings.TrimSpace(diagnostics.String())
	}()

	scanner := bufio.NewScanner(stdout)
	scanner.Buffer(make([]byte, 64*1024), 4*1024*1024)
	for scanner.Scan() {
		var record map[string]any
		if err := json.Unmarshal(scanner.Bytes(), &record); err != nil {
			s.addEvent(PiAssistantEvent{Type: "error", Error: "Could not read Pi response: " + err.Error()})
			continue
		}
		s.handleRecord(record)
	}
	scanErr := scanner.Err()
	waitErr := s.cmd.Wait()
	diagnostics := <-stderrDone
	os.Remove(s.contextPath)
	if scanErr != nil {
		s.addEvent(PiAssistantEvent{Type: "error", Error: "Pi output failed: " + scanErr.Error()})
	}
	if waitErr != nil {
		message := waitErr.Error()
		if diagnostics != "" {
			message += ": " + diagnostics
		}
		s.addEvent(PiAssistantEvent{Type: "error", Error: message})
	}
	s.addEvent(PiAssistantEvent{Type: "process_exit"})
}

func (s *piAssistantSession) handleRecord(record map[string]any) {
	typeName, _ := record["type"].(string)
	switch typeName {
	case "message_start":
		message := mapValue(record["message"])
		if message["role"] == "assistant" {
			s.addEvent(PiAssistantEvent{Type: "assistant_start"})
		}
	case "message_update":
		update := mapValue(record["assistantMessageEvent"])
		if update["type"] == "text_delta" {
			if delta, ok := update["delta"].(string); ok && delta != "" {
				s.addEvent(PiAssistantEvent{Type: "assistant_delta", Text: delta})
			}
		}
	case "message_end":
		message := mapValue(record["message"])
		if message["role"] == "assistant" {
			if errorMessage, ok := message["errorMessage"].(string); ok && errorMessage != "" {
				s.addEvent(PiAssistantEvent{Type: "error", Error: errorMessage})
			}
			text := messageText(message["content"])
			if text != "" {
				s.addEvent(PiAssistantEvent{Type: "assistant_end", Text: text})
			}
		}
	case "agent_start":
		s.addEvent(PiAssistantEvent{Type: "agent_start"})
	case "agent_settled":
		s.addEvent(PiAssistantEvent{Type: "agent_settled"})
	case "response":
		if success, ok := record["success"].(bool); ok && !success {
			text, _ := record["error"].(string)
			s.addEvent(PiAssistantEvent{Type: "error", Error: text})
		}
	case "auto_retry_start":
		s.addEvent(PiAssistantEvent{Type: "status", Text: "Retrying after a temporary model error…"})
	}
}

func (s *piAssistantSession) addEvent(event PiAssistantEvent) {
	s.eventsMu.Lock()
	defer s.eventsMu.Unlock()
	if len(s.events) >= 4000 {
		copy(s.events, s.events[len(s.events)-2000:])
		s.events = s.events[:2000]
	}
	s.events = append(s.events, event)
}

func mapValue(value any) map[string]any {
	if value, ok := value.(map[string]any); ok {
		return value
	}
	return map[string]any{}
}

func messageText(value any) string {
	if text, ok := value.(string); ok {
		return text
	}
	items, ok := value.([]any)
	if !ok {
		return ""
	}
	var text strings.Builder
	for _, item := range items {
		block := mapValue(item)
		if block["type"] == "text" {
			if part, ok := block["text"].(string); ok {
				text.WriteString(part)
			}
		}
	}
	return text.String()
}

func findPiExecutable() (string, error) {
	if path, err := exec.LookPath("pi"); err == nil {
		return path, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("find Pi CLI: %w", err)
	}
	candidates := []string{
		filepath.Join(home, ".pi", "agent", "bin", "pi"),
		filepath.Join(home, ".local", "bin", "pi"),
		"/opt/homebrew/bin/pi",
		"/usr/local/bin/pi",
	}
	if runtime.GOOS == "windows" {
		candidates = []string{filepath.Join(home, ".pi", "agent", "bin", "pi.exe"), filepath.Join(home, "AppData", "Roaming", "npm", "pi.cmd")}
	}
	for _, candidate := range candidates {
		if path, err := exec.LookPath(candidate); err == nil {
			return path, nil
		}
	}
	return "", fmt.Errorf("Pi CLI was not found; install Pi or add it to PATH")
}

func (s *DatabaseService) stopAllPiAssistants() {
	s.piMu.Lock()
	sessions := s.piSessions
	s.piSessions = make(map[string]*piAssistantSession)
	s.piMu.Unlock()
	for _, session := range sessions {
		session.writeMu.Lock()
		_ = session.stdin.Close()
		if session.cmd.Process != nil {
			_ = session.cmd.Process.Kill()
		}
		session.writeMu.Unlock()
		os.Remove(session.contextPath)
	}
}
