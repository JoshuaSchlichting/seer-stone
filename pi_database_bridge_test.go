package main

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"
)

func TestValidatePiReadOnlyQuery(t *testing.T) {
	accepted := []string{
		"SELECT 1",
		"WITH recent AS (SELECT id FROM events WHERE note = 'DELETE; UPDATE') SELECT * FROM recent",
		"SELECT 'drop table x; -- not SQL' AS message;",
		"SELECT 1 -- UPDATE records\n",
		"SHOW TABLES",
		"EXPLAIN SELECT * FROM orders",
	}
	for _, query := range accepted {
		if err := validatePiReadOnlyQuery(query); err != nil {
			t.Errorf("validatePiReadOnlyQuery(%q): %v", query, err)
		}
	}

	writeQueries := []string{"UPDATE accounts SET active = false", "INSERT INTO events (name) VALUES ('UPDATE')", "MERGE INTO target USING source ON target.id = source.id WHEN MATCHED THEN UPDATE SET target.name = source.name", "WITH removed AS (DELETE FROM accounts RETURNING id) SELECT * FROM removed"}
	for _, query := range writeQueries {
		if isWrite, err := validatePiQuery(query, true); err != nil || !isWrite {
			t.Errorf("validatePiQuery(%q, true) = (%v, %v), want (true, nil)", query, isWrite, err)
		}
	}

	rejected := []string{
		"UPDATE accounts SET active = false",
		"INSERT INTO events (name) VALUES ('safe'); DELETE FROM events",
		"SELECT * FROM accounts; DROP TABLE accounts",
		"WITH removed AS (DELETE FROM accounts RETURNING id) SELECT * FROM removed",
		"SELECT * INTO backup FROM accounts",
		"EXPLAIN ANALYZE SELECT * FROM accounts",
		"CALL refresh_cache()",
		"/* only a comment */",
	}
	for _, query := range rejected {
		if err := validatePiReadOnlyQuery(query); err == nil {
			t.Errorf("validatePiReadOnlyQuery(%q) unexpectedly succeeded", query)
		}
	}
}

func TestPiDatabaseBridgeRequiresCapabilityAndBlocksWrites(t *testing.T) {
	service := &DatabaseService{profiles: []ConnectionProfile{{ID: "profile", Database: "test", AllowPiDatabaseAccess: true}}}
	bridge, endpoint, err := startPiDatabaseBridge(service, "profile", "memory-only-password", "test", false, true)
	if err != nil {
		t.Fatal(err)
	}
	defer bridge.close()

	post := func(query, token string) int {
		body, err := json.Marshal(piDatabaseQueryRequest{Query: query})
		if err != nil {
			t.Fatal(err)
		}
		req, err := http.NewRequestWithContext(context.Background(), http.MethodPost, endpoint, strings.NewReader(string(body)))
		if err != nil {
			t.Fatal(err)
		}
		if token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		return resp.StatusCode
	}
	if status := post("SELECT 1", "wrong-token"); status != http.StatusUnauthorized {
		t.Fatalf("unauthorized request status = %d, want %d", status, http.StatusUnauthorized)
	}
	if status := post("UPDATE accounts SET active = false", bridge.token); status != http.StatusBadRequest {
		t.Fatalf("write query status = %d, want %d", status, http.StatusBadRequest)
	}
}

func TestPiDatabaseBridgeRequiresWriteApproval(t *testing.T) {
	service := &DatabaseService{profiles: []ConnectionProfile{{ID: "profile", Database: "test", AllowPiDatabaseAccess: true, AllowPiDatabaseWrite: true}}}
	bridge, endpoint, err := startPiDatabaseBridge(service, "profile", "memory-only-password", "test", true, true)
	if err != nil {
		t.Fatal(err)
	}
	defer bridge.close()
	approval := make(chan string, 1)
	bridge.onApproval = func(id, _ string) { approval <- id }
	body, err := json.Marshal(piDatabaseQueryRequest{Query: "UPDATE accounts SET active = false"})
	if err != nil {
		t.Fatal(err)
	}
	req, err := http.NewRequest(http.MethodPost, endpoint, strings.NewReader(string(body)))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+bridge.token)
	response := make(chan int, 1)
	go func() {
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			response <- 0
			return
		}
		defer resp.Body.Close()
		response <- resp.StatusCode
	}()
	var approvalID string
	select {
	case approvalID = <-approval:
	case <-time.After(2 * time.Second):
		t.Fatal("write request did not ask for approval")
	}
	if !bridge.resolveApproval(approvalID, false) {
		t.Fatal("could not resolve the pending write approval")
	}
	select {
	case status := <-response:
		if status != http.StatusForbidden {
			t.Fatalf("denied write status = %d, want %d", status, http.StatusForbidden)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("write request did not finish after denial")
	}
}
