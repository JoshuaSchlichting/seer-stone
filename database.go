package main

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/stdlib"
)

type ConnectionProfile struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	Host     string `json:"host"`
	Port     string `json:"port"`
	Database string `json:"database"`
	Username string `json:"username"`
	SSLMode  string `json:"sslMode"`
}

type QueryResult struct {
	Columns    []string `json:"columns"`
	Rows       [][]any  `json:"rows"`
	RowCount   int64    `json:"rowCount"`
	CommandTag string   `json:"commandTag"`
	DurationMs int64    `json:"durationMs"`
}

type DatabaseService struct {
	mu             sync.RWMutex
	profiles       []ConnectionProfile
	localPasswords map[string]string
	localIDs       map[string]bool
	file           string
}

func NewDatabaseService() (*DatabaseService, error) {
	configDir, err := os.UserConfigDir()
	if err != nil {
		return nil, fmt.Errorf("find user config directory: %w", err)
	}
	dir := filepath.Join(configDir, "seer-stone")
	if err := os.MkdirAll(dir, 0700); err != nil {
		return nil, fmt.Errorf("create config directory: %w", err)
	}
	service := &DatabaseService{
		file:           filepath.Join(dir, "connections.json"),
		localPasswords: make(map[string]string),
		localIDs:       make(map[string]bool),
	}
	data, err := os.ReadFile(service.file)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("read connection profiles: %w", err)
	}
	if len(data) > 0 {
		if err := json.Unmarshal(data, &service.profiles); err != nil {
			return nil, fmt.Errorf("parse connection profiles: %w", err)
		}
	}
	if service.profiles == nil {
		service.profiles = []ConnectionProfile{}
	}
	if err := service.loadLocalConnections("connections.local.json"); err != nil {
		return nil, err
	}
	return service, nil
}

func (s *DatabaseService) Profiles() []ConnectionProfile {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return append([]ConnectionProfile{}, s.profiles...)
}

// LocalPassword returns credentials loaded from the gitignored local connection file.
func (s *DatabaseService) LocalPassword(id string) string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.localPasswords[id]
}

func (s *DatabaseService) SaveProfile(profile ConnectionProfile) (ConnectionProfile, error) {
	profile.Name = strings.TrimSpace(profile.Name)
	profile.Host = strings.TrimSpace(profile.Host)
	profile.Database = strings.TrimSpace(profile.Database)
	profile.Username = strings.TrimSpace(profile.Username)
	profile.Port = strings.TrimSpace(profile.Port)
	profile.SSLMode = strings.TrimSpace(profile.SSLMode)
	if profile.Name == "" || profile.Host == "" || profile.Database == "" || profile.Username == "" {
		return ConnectionProfile{}, errors.New("name, host, database, and username are required")
	}
	if profile.Port == "" {
		profile.Port = "26257"
	}
	if profile.SSLMode == "" {
		profile.SSLMode = "verify-full"
	}
	if profile.ID == "" {
		var id [16]byte
		if _, err := rand.Read(id[:]); err != nil {
			return ConnectionProfile{}, fmt.Errorf("create profile ID: %w", err)
		}
		profile.ID = hex.EncodeToString(id[:])
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	found := false
	for i := range s.profiles {
		if s.profiles[i].ID == profile.ID {
			s.profiles[i] = profile
			found = true
			break
		}
	}
	if !found {
		s.profiles = append(s.profiles, profile)
	}
	if err := s.persistLocked(); err != nil {
		return ConnectionProfile{}, err
	}
	return profile, nil
}

func (s *DatabaseService) DeleteProfile(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.localIDs[id] {
		return errors.New("edit this connection in connections.local.json")
	}
	for i, profile := range s.profiles {
		if profile.ID == id {
			s.profiles = append(s.profiles[:i], s.profiles[i+1:]...)
			return s.persistLocked()
		}
	}
	return errors.New("connection profile not found")
}

func (s *DatabaseService) persistLocked() error {
	profiles := make([]ConnectionProfile, 0, len(s.profiles))
	for _, profile := range s.profiles {
		if !s.localIDs[profile.ID] {
			profiles = append(profiles, profile)
		}
	}
	data, err := json.MarshalIndent(profiles, "", "  ")
	if err != nil {
		return fmt.Errorf("encode connection profiles: %w", err)
	}
	if err := os.WriteFile(s.file, data, 0600); err != nil {
		return fmt.Errorf("save connection profiles: %w", err)
	}
	return nil
}

func (s *DatabaseService) TestConnection(id, password string) (string, error) {
	profile, err := s.profile(id)
	if err != nil {
		return "", err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 12*time.Second)
	defer cancel()
	db, err := openDatabase(profile, password)
	if err != nil {
		return "", err
	}
	defer db.Close()
	if err := db.PingContext(ctx); err != nil {
		return "", fmt.Errorf("connection failed: %w", err)
	}
	var version string
	if err := db.QueryRowContext(ctx, "SELECT version()").Scan(&version); err != nil {
		return "", fmt.Errorf("connected, but could not read server version: %w", err)
	}
	return version, nil
}

func (s *DatabaseService) RunQuery(id, password, query string) (QueryResult, error) {
	profile, err := s.profile(id)
	if err != nil {
		return QueryResult{}, err
	}
	query = strings.TrimSpace(query)
	if query == "" {
		return QueryResult{}, errors.New("enter a SQL statement first")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	db, err := openDatabase(profile, password)
	if err != nil {
		return QueryResult{}, err
	}
	defer db.Close()

	started := time.Now()
	rows, err := db.QueryContext(ctx, query)
	if err != nil {
		return QueryResult{}, fmt.Errorf("query failed: %w", err)
	}
	defer rows.Close()
	columns, err := rows.Columns()
	if err != nil {
		return QueryResult{}, fmt.Errorf("read result columns: %w", err)
	}
	result := QueryResult{Columns: columns, Rows: make([][]any, 0), CommandTag: "query"}
	for rows.Next() {
		values := make([]any, len(columns))
		destinations := make([]any, len(columns))
		for i := range values {
			destinations[i] = &values[i]
		}
		if err := rows.Scan(destinations...); err != nil {
			return QueryResult{}, fmt.Errorf("read query result: %w", err)
		}
		for i, value := range values {
			if bytes, ok := value.([]byte); ok {
				values[i] = string(bytes)
			}
		}
		result.Rows = append(result.Rows, values)
	}
	if err := rows.Err(); err != nil {
		return QueryResult{}, fmt.Errorf("read query result: %w", err)
	}
	result.RowCount = int64(len(result.Rows))
	result.DurationMs = time.Since(started).Milliseconds()
	return result, nil
}

func (s *DatabaseService) profile(id string) (ConnectionProfile, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, profile := range s.profiles {
		if profile.ID == id {
			return profile, nil
		}
	}
	return ConnectionProfile{}, errors.New("connection profile not found")
}

func (s *DatabaseService) loadLocalConnections(path string) error {
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("read %s: %w", path, err)
	}
	var config struct {
		Connections []struct {
			Name  string `json:"name"`
			DBURI string `json:"dbURI"`
		} `json:"connections"`
	}
	if err := json.Unmarshal(data, &config); err != nil {
		return fmt.Errorf("parse %s: %w", path, err)
	}
	for _, entry := range config.Connections {
		u, err := url.Parse(entry.DBURI)
		if err != nil || u.Scheme != "postgresql" && u.Scheme != "postgres" || u.Hostname() == "" {
			return fmt.Errorf("invalid dbURI in %s for connection %q", path, entry.Name)
		}
		username := ""
		password := ""
		if u.User != nil {
			username = u.User.Username()
			password, _ = u.User.Password()
		}
		port := u.Port()
		if port == "" {
			port = "26257"
		}
		sslMode := u.Query().Get("sslmode")
		if sslMode == "" {
			sslMode = "verify-full"
		}
		hash := sha256.Sum256([]byte(entry.DBURI))
		id := "local-" + hex.EncodeToString(hash[:8])
		profile := ConnectionProfile{
			ID: id, Name: entry.Name, Host: u.Hostname(), Port: port,
			Database: strings.TrimPrefix(u.Path, "/"), Username: username, SSLMode: sslMode,
		}
		if profile.Name == "" || profile.Database == "" || profile.Username == "" || password == "" {
			return fmt.Errorf("connection %q in %s is missing a name or required dbURI fields", entry.Name, path)
		}
		s.profiles = append(s.profiles, profile)
		s.localPasswords[id] = password
		s.localIDs[id] = true
	}
	return nil
}

func openDatabase(profile ConnectionProfile, password string) (*sql.DB, error) {
	port := profile.Port
	if port == "" {
		port = "26257"
	}
	sslMode := profile.SSLMode
	if sslMode == "" {
		sslMode = "verify-full"
	}
	u := &url.URL{
		Scheme: "postgresql",
		User:   url.UserPassword(profile.Username, password),
		Host:   fmt.Sprintf("%s:%s", profile.Host, port),
		Path:   profile.Database,
	}
	params := url.Values{}
	params.Set("sslmode", sslMode)
	u.RawQuery = params.Encode()
	config, err := pgx.ParseConfig(u.String())
	if err != nil {
		return nil, fmt.Errorf("invalid connection settings: %w", err)
	}
	return stdlib.OpenDB(*config), nil
}
