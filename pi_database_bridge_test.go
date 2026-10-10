package main

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
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

	rejected := []string{
		"UPDATE accounts SET active = false",
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
	bridge, endpoint, err := startPiDatabaseBridge(service, "profile", "memory-only-password", "test")
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
