package main

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

const (
	piDatabaseToolName = "seer_stone_query_database"
	piMaxQueryBytes    = 32 * 1024
	piMaxRows          = 200
	piMaxColumns       = 50
	piMaxCellBytes     = 8 * 1024
	piMaxResultBytes   = 512 * 1024
)

type piDatabaseBridge struct {
	service   *DatabaseService
	profileID string
	password  string
	database  string
	token     string
	listener  net.Listener
	server    *http.Server
	ctx       context.Context
	cancel    context.CancelFunc
	queryGate chan struct{}
}

type piDatabaseQueryRequest struct {
	Query string `json:"query"`
}

type piDatabaseQueryResponse struct {
	Columns   []string `json:"columns,omitempty"`
	Rows      [][]any  `json:"rows,omitempty"`
	RowCount  int      `json:"rowCount,omitempty"`
	Truncated bool     `json:"truncated,omitempty"`
	Error     string   `json:"error,omitempty"`
}

func startPiDatabaseBridge(service *DatabaseService, profileID, password, database string) (*piDatabaseBridge, string, error) {
	profile, err := service.profileForDatabase(profileID, database)
	if err != nil {
		return nil, "", err
	}
	if !profile.AllowPiDatabaseAccess {
		return nil, "", errors.New("database access for Pi is disabled for this connection")
	}
	if strings.TrimSpace(password) == "" {
		return nil, "", errors.New("connect to this database before granting Pi access")
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, "", fmt.Errorf("start local Pi database bridge: %w", err)
	}
	var tokenBytes [32]byte
	if _, err := rand.Read(tokenBytes[:]); err != nil {
		listener.Close()
		return nil, "", fmt.Errorf("create local Pi bridge token: %w", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	bridge := &piDatabaseBridge{
		service: service, profileID: profileID, password: password, database: profile.Database,
		token: hex.EncodeToString(tokenBytes[:]), listener: listener, ctx: ctx, cancel: cancel,
		queryGate: make(chan struct{}, 1),
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/query", bridge.handleQuery)
	bridge.server = &http.Server{
		Handler:           mux,
		ReadHeaderTimeout: 3 * time.Second,
		ReadTimeout:       10 * time.Second,
		WriteTimeout:      35 * time.Second,
		MaxHeaderBytes:    8 * 1024,
	}
	go func() {
		_ = bridge.server.Serve(listener)
	}()
	endpoint := "http://" + listener.Addr().String() + "/query"
	return bridge, endpoint, nil
}

func (b *piDatabaseBridge) handleQuery(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		http.Error(w, `{"error":"POST is required"}`, http.StatusMethodNotAllowed)
		return
	}
	if !b.authorized(r) {
		http.Error(w, `{"error":"unauthorized"}`, http.StatusUnauthorized)
		return
	}
	if r.ContentLength > piMaxQueryBytes+1024 {
		http.Error(w, `{"error":"request is too large"}`, http.StatusRequestEntityTooLarge)
		return
	}
	defer r.Body.Close()
	var request piDatabaseQueryRequest
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, piMaxQueryBytes+1024))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&request); err != nil {
		http.Error(w, `{"error":"invalid query request"}`, http.StatusBadRequest)
		return
	}
	if err := validatePiReadOnlyQuery(request.Query); err != nil {
		writePiBridgeJSON(w, http.StatusBadRequest, piDatabaseQueryResponse{Error: err.Error()})
		return
	}
	select {
	case b.queryGate <- struct{}{}:
		defer func() { <-b.queryGate }()
	case <-r.Context().Done():
		return
	case <-b.ctx.Done():
		writePiBridgeJSON(w, http.StatusGone, piDatabaseQueryResponse{Error: "Pi database access has been revoked"})
		return
	default:
		writePiBridgeJSON(w, http.StatusTooManyRequests, piDatabaseQueryResponse{Error: "another Pi query is already running"})
		return
	}
	result, err := b.service.runPiReadOnlyQuery(b.ctx, b.profileID, b.password, b.database, request.Query)
	if err != nil {
		status := http.StatusBadRequest
		if errors.Is(err, context.DeadlineExceeded) {
			status = http.StatusGatewayTimeout
		}
		writePiBridgeJSON(w, status, piDatabaseQueryResponse{Error: err.Error()})
		return
	}
	writePiBridgeJSON(w, http.StatusOK, result)
}

func (b *piDatabaseBridge) authorized(r *http.Request) bool {
	if r.URL.Path != "/query" || r.URL.RawQuery != "" {
		return false
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil || net.ParseIP(host) == nil || !net.ParseIP(host).IsLoopback() {
		return false
	}
	provided := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	return len(provided) == len(b.token) && subtle.ConstantTimeCompare([]byte(provided), []byte(b.token)) == 1
}

func (b *piDatabaseBridge) close() {
	b.cancel()
	_ = b.server.Close()
	_ = b.listener.Close()
}

func writePiBridgeJSON(w http.ResponseWriter, status int, payload piDatabaseQueryResponse) {
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(payload)
}

func (s *DatabaseService) runPiReadOnlyQuery(parent context.Context, profileID, password, database, query string) (piDatabaseQueryResponse, error) {
	profile, err := s.profileForDatabase(profileID, database)
	if err != nil {
		return piDatabaseQueryResponse{}, err
	}
	if !profile.AllowPiDatabaseAccess {
		return piDatabaseQueryResponse{}, errors.New("Pi database access has been disabled")
	}
	if err := validatePiReadOnlyQuery(query); err != nil {
		return piDatabaseQueryResponse{}, err
	}
	db, err := s.database(profile, password)
	if err != nil {
		return piDatabaseQueryResponse{}, err
	}
	ctx, cancel := context.WithTimeout(parent, 30*time.Second)
	defer cancel()
	started := time.Now()
	var rows *sql.Rows
	if profile.Engine == "snowflake" {
		// gosnowflake does not support read-only transactions; SQL is still constrained
		// to a single, read-only statement by the validator below.
		rows, err = db.QueryContext(ctx, query)
	} else {
		tx, txErr := db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
		if txErr != nil {
			return piDatabaseQueryResponse{}, fmt.Errorf("start read-only transaction: %w", txErr)
		}
		defer tx.Rollback()
		rows, err = tx.QueryContext(ctx, query)
	}
	if err != nil {
		return piDatabaseQueryResponse{}, fmt.Errorf("read-only query failed: %w", err)
	}
	defer rows.Close()
	columns, err := rows.Columns()
	if err != nil {
		return piDatabaseQueryResponse{}, fmt.Errorf("read query columns: %w", err)
	}
	if len(columns) > piMaxColumns {
		return piDatabaseQueryResponse{}, fmt.Errorf("query returned %d columns; Pi results are limited to %d", len(columns), piMaxColumns)
	}
	for i := range columns {
		columns[i] = truncatePiText(columns[i], 512)
	}
	response := piDatabaseQueryResponse{Columns: columns, Rows: make([][]any, 0, piMaxRows)}
	for rows.Next() {
		if len(response.Rows) >= piMaxRows {
			response.Truncated = true
			break
		}
		values := make([]any, len(columns))
		destinations := make([]any, len(columns))
		for i := range values {
			destinations[i] = &values[i]
		}
		if err := rows.Scan(destinations...); err != nil {
			return piDatabaseQueryResponse{}, fmt.Errorf("read query row: %w", err)
		}
		for i, value := range values {
			switch cell := value.(type) {
			case []byte:
				values[i] = truncatePiText(string(cell), piMaxCellBytes)
			case string:
				values[i] = truncatePiText(cell, piMaxCellBytes)
			case nil, bool, int64, float64, time.Time:
			default:
				values[i] = truncatePiText(fmt.Sprint(cell), piMaxCellBytes)
			}
		}
		response.Rows = append(response.Rows, values)
	}
	if err := rows.Err(); err != nil {
		return piDatabaseQueryResponse{}, fmt.Errorf("read query rows: %w", err)
	}
	response.RowCount = len(response.Rows)
	if time.Since(started) > 30*time.Second {
		return piDatabaseQueryResponse{}, context.DeadlineExceeded
	}
	for len(response.Rows) > 0 {
		encoded, err := json.Marshal(response)
		if err != nil {
			return piDatabaseQueryResponse{}, fmt.Errorf("encode query results: %w", err)
		}
		if len(encoded) <= piMaxResultBytes {
			break
		}
		response.Rows = response.Rows[:len(response.Rows)-1]
		response.RowCount = len(response.Rows)
		response.Truncated = true
	}
	return response, nil
}

func truncatePiText(value string, max int) string {
	if len(value) <= max {
		return value
	}
	value = value[:max]
	for !utf8.ValidString(value) {
		value = value[:len(value)-1]
	}
	return value + "…"
}

func validatePiReadOnlyQuery(query string) error {
	if strings.TrimSpace(query) == "" {
		return errors.New("enter a SQL query")
	}
	if len(query) > piMaxQueryBytes {
		return fmt.Errorf("query exceeds the %d-byte limit", piMaxQueryBytes)
	}
	plain, err := scrubPiSQL(query)
	if err != nil {
		return err
	}
	trimmed := strings.TrimSpace(plain)
	if trimmed == "" {
		return errors.New("query contains no SQL statement")
	}
	if strings.Contains(trimmed, ";") {
		trimmed = strings.TrimSpace(strings.TrimSuffix(trimmed, ";"))
		if strings.Contains(trimmed, ";") {
			return errors.New("only one SQL statement is allowed")
		}
	}
	fields := strings.FieldsFunc(strings.ToUpper(trimmed), func(r rune) bool {
		return !(unicode.IsLetter(r) || unicode.IsDigit(r) || r == '_' || r == '$')
	})
	if len(fields) == 0 {
		return errors.New("query contains no SQL statement")
	}
	allowedStart := map[string]bool{"SELECT": true, "WITH": true, "SHOW": true, "DESCRIBE": true, "DESC": true, "EXPLAIN": true, "VALUES": true, "TABLE": true}
	if !allowedStart[fields[0]] {
		return errors.New("Pi's database bridge only permits read-only SQL queries")
	}
	blocked := map[string]bool{
		"INSERT": true, "UPDATE": true, "DELETE": true, "UPSERT": true, "MERGE": true,
		"CREATE": true, "ALTER": true, "DROP": true, "TRUNCATE": true, "GRANT": true, "REVOKE": true, "DENY": true,
		"CALL": true, "EXEC": true, "EXECUTE": true, "COPY": true, "PUT": true, "GET": true, "REMOVE": true,
		"UNDROP": true, "REFRESH": true, "COMMIT": true, "ROLLBACK": true, "BEGIN": true, "START": true,
		"TRANSACTION": true, "SET": true, "RESET": true, "USE": true, "ATTACH": true, "DETACH": true,
		"VACUUM": true, "ANALYZE": true, "KILL": true, "INTO": true,
	}
	for _, field := range fields {
		if blocked[field] {
			return fmt.Errorf("Pi's database bridge blocks %s statements", strings.ToLower(field))
		}
	}
	if fields[0] == "EXPLAIN" {
		for _, field := range fields[1:] {
			if field == "ANALYZE" {
				return errors.New("EXPLAIN ANALYZE is not allowed through Pi's database bridge")
			}
		}
	}
	return nil
}

func scrubPiSQL(query string) (string, error) {
	var out strings.Builder
	out.Grow(len(query))
	for i := 0; i < len(query); {
		if i+1 < len(query) && query[i:i+2] == "--" {
			for i < len(query) && query[i] != '\n' {
				out.WriteByte(' ')
				i++
			}
			continue
		}
		if i+1 < len(query) && query[i:i+2] == "/*" {
			depth := 1
			out.WriteString("  ")
			i += 2
			for i < len(query) && depth > 0 {
				switch {
				case i+1 < len(query) && query[i:i+2] == "/*":
					depth++
					out.WriteString("  ")
					i += 2
				case i+1 < len(query) && query[i:i+2] == "*/":
					depth--
					out.WriteString("  ")
					i += 2
				default:
					if query[i] == '\n' {
						out.WriteByte('\n')
					} else {
						out.WriteByte(' ')
					}
					i++
				}
			}
			if depth != 0 {
				return "", errors.New("unterminated SQL comment")
			}
			continue
		}
		if query[i] == '$' {
			if delimiter, ok := piDollarDelimiter(query[i:]); ok {
				end := strings.Index(query[i+len(delimiter):], delimiter)
				if end < 0 {
					return "", errors.New("unterminated SQL string")
				}
				length := len(delimiter)*2 + end
				out.WriteString(strings.Repeat(" ", length))
				i += length
				continue
			}
		}
		if query[i] == '\'' || query[i] == '"' || query[i] == '`' {
			quote := query[i]
			out.WriteByte(' ')
			i++
			closed := false
			for i < len(query) {
				if query[i] == quote {
					if i+1 < len(query) && query[i+1] == quote {
						out.WriteString("  ")
						i += 2
						continue
					}
					out.WriteByte(' ')
					i++
					closed = true
					break
				}
				if quote == '\'' && query[i] == '\\' && i+1 < len(query) {
					out.WriteString("  ")
					i += 2
					continue
				}
				if query[i] == '\n' {
					out.WriteByte('\n')
				} else {
					out.WriteByte(' ')
				}
				i++
			}
			if !closed {
				return "", errors.New("unterminated SQL quoted value or identifier")
			}
			continue
		}
		out.WriteByte(query[i])
		i++
	}
	return out.String(), nil
}

func piDollarDelimiter(value string) (string, bool) {
	if len(value) < 2 || value[0] != '$' {
		return "", false
	}
	for i := 1; i < len(value); i++ {
		if value[i] == '$' {
			return value[:i+1], true
		}
		c := value[i]
		if !((c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9') || c == '_') {
			return "", false
		}
	}
	return "", false
}

func piExtensionSource(endpoint, token string) ([]byte, error) {
	endpointJSON, err := json.Marshal(endpoint)
	if err != nil {
		return nil, err
	}
	tokenJSON, err := json.Marshal(token)
	if err != nil {
		return nil, err
	}
	source := fmt.Sprintf(`import { Type } from "@earendil-works/pi-ai";

const endpoint = %s;
const token = %s;

export default function (pi) {
  pi.registerTool({
    name: %q,
    label: "Query database",
    description: "Run one bounded, read-only SQL query through Seer Stone. Results are limited to 200 rows. Use this to inspect schema/data and ground SQL answers. Writes and multiple statements are blocked.",
    parameters: Type.Object({ query: Type.String({ description: "One read-only SQL query for the selected database." }) }),
    async execute(_toolCallId, params, signal) {
      const response = await fetch(endpoint, {
        method: "POST",
        headers: { "Content-Type": "application/json", "Authorization": "Bearer " + token },
        body: JSON.stringify({ query: params.query }),
        signal,
      });
      const result = await response.json();
      if (!response.ok) throw new Error(result.error || ("Seer Stone query failed (" + response.status + ")"));
      return { content: [{ type: "text", text: JSON.stringify(result) }], details: result };
    },
  });
}
`, string(endpointJSON), string(tokenJSON), piDatabaseToolName)
	return []byte(source), nil
}

func createPiDatabaseExtension(endpoint, token string) (string, error) {
	source, err := piExtensionSource(endpoint, token)
	if err != nil {
		return "", err
	}
	file, err := os.CreateTemp("", "seer-stone-pi-db-*.ts")
	if err != nil {
		return "", fmt.Errorf("create Pi database tool: %w", err)
	}
	path := file.Name()
	if _, err := file.Write(source); err != nil {
		file.Close()
		os.Remove(path)
		return "", fmt.Errorf("write Pi database tool: %w", err)
	}
	if err := file.Close(); err != nil {
		os.Remove(path)
		return "", fmt.Errorf("close Pi database tool: %w", err)
	}
	return path, nil
}
