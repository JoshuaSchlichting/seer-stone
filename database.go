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
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/99designs/keyring"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/stdlib"
	"github.com/snowflakedb/gosnowflake"
	"github.com/wailsapp/wails/v3/pkg/application"
)

type ConnectionProfile struct {
	ID                    string `json:"id"`
	ProjectDirectory      string `json:"projectDirectory,omitempty"`
	Name                  string `json:"name"`
	Engine                string `json:"engine"`
	Host                  string `json:"host"`
	Port                  string `json:"port"`
	Database              string `json:"database"`
	Schema                string `json:"schema"`
	Warehouse             string `json:"warehouse"`
	Role                  string `json:"role"`
	Username              string `json:"username"`
	SSLMode               string `json:"sslMode"`
	AllowPiDatabaseAccess bool   `json:"allowPiDatabaseAccess"`
	AllowPiDatabaseWrite  bool   `json:"allowPiDatabaseWrite"`
}

type localPiDatabasePermissions struct {
	AllowPiDatabaseAccess bool `json:"allowPiDatabaseAccess"`
	AllowPiDatabaseWrite  bool `json:"allowPiDatabaseWrite"`
}

type QueryResult struct {
	Columns    []string `json:"columns"`
	Rows       [][]any  `json:"rows"`
	RowCount   int64    `json:"rowCount"`
	CommandTag string   `json:"commandTag"`
	DurationMs int64    `json:"durationMs"`
}

type DatabaseSchema struct {
	Name    string           `json:"name"`
	Objects []DatabaseObject `json:"objects"`
}

type DatabaseObject struct {
	Name    string           `json:"name"`
	Kind    string           `json:"kind"`
	Columns []DatabaseColumn `json:"columns"`
}

type DatabaseColumn struct {
	Name     string `json:"name"`
	DataType string `json:"dataType"`
	Nullable bool   `json:"nullable"`
	Default  string `json:"default"`
}

type databasePool struct {
	db          *sql.DB
	fingerprint [32]byte
}

type DatabaseService struct {
	mu                     sync.RWMutex
	profiles               []ConnectionProfile
	localPasswords         map[string]string
	credentialStore        keyring.Keyring
	localIDs               map[string]bool
	localPiPermissionsFile string
	localPiPermissions     map[string]localPiDatabasePermissions
	localProjectDirsFile   string
	localProjectDirs       map[string]string
	pools                  map[string]databasePool
	piMu                   sync.Mutex
	piSessions             map[string]*piAssistantSession
	file                   string
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
	credentialStore, err := keyring.Open(keyring.Config{ServiceName: "Seer Stone", AllowedBackends: []keyring.BackendType{keyring.KeychainBackend, keyring.SecretServiceBackend, keyring.WinCredBackend}})
	if err != nil {
		return nil, fmt.Errorf("open system credential store: %w", err)
	}
	service := &DatabaseService{
		credentialStore:        credentialStore,
		file:                   filepath.Join(dir, "connections.json"),
		localPiPermissionsFile: filepath.Join(dir, "pi-permissions.json"),
		localPiPermissions:     make(map[string]localPiDatabasePermissions),
		localProjectDirsFile:   filepath.Join(dir, "project-directories.json"),
		localProjectDirs:       make(map[string]string),
		localPasswords:         make(map[string]string),
		localIDs:               make(map[string]bool),
		pools:                  make(map[string]databasePool),
		piSessions:             make(map[string]*piAssistantSession),
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
	if err := service.loadLocalPiPermissions(); err != nil {
		return nil, err
	}
	if err := service.loadLocalConnections("connections.local.json"); err != nil {
		return nil, err
	}
	if err := service.loadLocalProjectDirectories(); err != nil {
		return nil, err
	}
	return service, nil
}

func (s *DatabaseService) SaveCSVFile(contents string) error {
	path, err := application.Get().Dialog.SaveFile().SetFilename("results.csv").AddFilter("CSV files", "*.csv").PromptForSingleSelection()
	if err != nil || path == "" {
		return err
	}
	return os.WriteFile(path, []byte("\uFEFF"+contents), 0600)
}

func (s *DatabaseService) ProjectSQLFiles(profileID string) ([]string, error) {
	profile, err := s.profile(profileID)
	if err != nil {
		return nil, err
	}
	if profile.ProjectDirectory == "" {
		return []string{}, nil
	}
	files := make([]string, 0)
	err = filepath.WalkDir(profile.ProjectDirectory, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() {
			if path != profile.ProjectDirectory && (entry.Name() == ".git" || entry.Name() == "node_modules") {
				return filepath.SkipDir
			}
			return nil
		}
		if strings.EqualFold(filepath.Ext(entry.Name()), ".sql") {
			rel, e := filepath.Rel(profile.ProjectDirectory, path)
			if e != nil {
				return e
			}
			files = append(files, filepath.ToSlash(rel))
		}
		return nil
	})
	sort.Strings(files)
	return files, err
}

func (s *DatabaseService) ReadProjectSQL(profileID, name string) (string, error) {
	path, err := s.projectSQLPath(profileID, name)
	if err != nil {
		return "", err
	}
	data, err := os.ReadFile(path)
	return string(data), err
}

func (s *DatabaseService) WriteProjectSQL(profileID, name, content string) error {
	path, err := s.projectSQLPath(profileID, name)
	if err != nil {
		return err
	}
	return os.WriteFile(path, []byte(content), 0600)
}

func (s *DatabaseService) projectSQLPath(profileID, name string) (string, error) {
	profile, err := s.profile(profileID)
	if err != nil {
		return "", err
	}
	if profile.ProjectDirectory == "" || filepath.Ext(name) != ".sql" || filepath.IsAbs(name) {
		return "", errors.New("invalid SQL project file")
	}
	root, err := filepath.Abs(profile.ProjectDirectory)
	if err != nil {
		return "", err
	}
	path, err := filepath.Abs(filepath.Join(root, filepath.FromSlash(name)))
	if err != nil {
		return "", err
	}
	rel, err := filepath.Rel(root, path)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", errors.New("SQL file is outside the project folder")
	}
	return path, nil
}

func (s *DatabaseService) ChooseProjectDirectory(profileID string) (string, error) {
	profile, err := s.profile(profileID)
	if err != nil {
		return "", err
	}
	result, err := application.Get().Dialog.OpenFile().CanChooseDirectories(true).CanChooseFiles(false).SetTitle("Choose project folder for " + profile.Name).SetDirectory(profile.ProjectDirectory).PromptForSingleSelection()
	if err != nil {
		return "", err
	}
	if result == "" {
		return "", nil
	}
	profile.ProjectDirectory = result
	s.mu.Lock()
	if s.localIDs[profile.ID] {
		s.localProjectDirs[profile.ID] = result
		err = s.persistLocalProjectDirectoriesLocked()
	}
	s.mu.Unlock()
	if err != nil {
		return "", err
	}
	_, err = s.SaveProfile(profile)
	return result, err
}

func (s *DatabaseService) Profiles() []ConnectionProfile {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return append([]ConnectionProfile{}, s.profiles...)
}

// LocalPassword returns credentials loaded from the gitignored local connection file or system keyring.
func (s *DatabaseService) LocalPassword(id string) string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if password := s.localPasswords[id]; password != "" {
		return password
	}
	item, err := s.credentialStore.Get("connection:" + id)
	if err != nil {
		return ""
	}
	return string(item.Data)
}

func (s *DatabaseService) HasCredential(id string) bool {
	_, err := s.credentialStore.Get("connection:" + id)
	return err == nil
}

func (s *DatabaseService) SaveCredential(id, password string) error {
	if password == "" {
		return errors.New("password cannot be empty")
	}
	return s.credentialStore.Set(keyring.Item{Key: "connection:" + id, Label: "Seer Stone database password", Data: []byte(password)})
}

func (s *DatabaseService) DeleteCredential(id string) error {
	err := s.credentialStore.Remove("connection:" + id)
	if errors.Is(err, keyring.ErrKeyNotFound) {
		return nil
	}
	return err
}

func (s *DatabaseService) SaveProfile(profile ConnectionProfile) (ConnectionProfile, error) {
	profile.Name = strings.TrimSpace(profile.Name)
	profile.ProjectDirectory = strings.TrimSpace(profile.ProjectDirectory)
	profile.Engine = strings.ToLower(strings.TrimSpace(profile.Engine))
	if profile.Engine == "" {
		profile.Engine = "cockroach"
	}
	if profile.Engine != "cockroach" && profile.Engine != "postgres" && profile.Engine != "snowflake" {
		return ConnectionProfile{}, errors.New("engine must be cockroach, postgres, or snowflake")
	}
	profile.Host = strings.TrimSpace(profile.Host)
	profile.Schema = strings.TrimSpace(profile.Schema)
	profile.Warehouse = strings.TrimSpace(profile.Warehouse)
	profile.Role = strings.TrimSpace(profile.Role)
	profile.Database = strings.TrimSpace(profile.Database)
	profile.Username = strings.TrimSpace(profile.Username)
	profile.Port = strings.TrimSpace(profile.Port)
	profile.SSLMode = strings.TrimSpace(profile.SSLMode)
	if profile.Name == "" || profile.Host == "" || profile.Username == "" || (profile.Engine != "snowflake" && profile.Database == "") {
		return ConnectionProfile{}, errors.New("name, host/account, username, and database (except for Snowflake) are required")
	}
	if profile.AllowPiDatabaseWrite && !profile.AllowPiDatabaseAccess {
		return ConnectionProfile{}, errors.New("Pi write access requires Pi database access to be enabled")
	}
	if profile.Port == "" {
		if profile.Engine == "postgres" {
			profile.Port = "5432"
		} else if profile.Engine == "cockroach" {
			profile.Port = "26257"
		}
	}
	if profile.SSLMode == "" && profile.Engine != "snowflake" {
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
	if profile.ID != "" {
		s.stopPiAssistantsForProfileLocked(profile.ID)
	}
	found := false
	for i := range s.profiles {
		if s.profiles[i].ID == profile.ID {
			if !sameDatabaseConnectionSettings(s.profiles[i], profile) {
				s.closeProfilePoolsLocked(profile.ID)
			}
			s.profiles[i] = profile
			found = true
			break
		}
	}
	if !found {
		s.profiles = append(s.profiles, profile)
	}
	if s.localIDs[profile.ID] {
		if s.localPiPermissions == nil {
			s.localPiPermissions = make(map[string]localPiDatabasePermissions)
		}
		if profile.AllowPiDatabaseAccess || profile.AllowPiDatabaseWrite {
			s.localPiPermissions[profile.ID] = localPiDatabasePermissions{
				AllowPiDatabaseAccess: profile.AllowPiDatabaseAccess,
				AllowPiDatabaseWrite:  profile.AllowPiDatabaseWrite,
			}
		} else {
			delete(s.localPiPermissions, profile.ID)
		}
	}
	if err := s.persistLocked(); err != nil {
		return ConnectionProfile{}, err
	}
	return profile, nil
}

func sameDatabaseConnectionSettings(a, b ConnectionProfile) bool {
	return a.Engine == b.Engine && a.Host == b.Host && a.Port == b.Port &&
		a.Database == b.Database && a.Schema == b.Schema && a.Warehouse == b.Warehouse &&
		a.Role == b.Role && a.Username == b.Username && a.SSLMode == b.SSLMode
}

func (s *DatabaseService) DeleteProfile(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.localIDs[id] {
		return errors.New("edit this connection in connections.local.json")
	}
	for i, profile := range s.profiles {
		if profile.ID == id {
			s.stopPiAssistantsForProfileLocked(id)
			s.closeProfilePoolsLocked(id)
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
	return s.persistLocalPiPermissionsLocked()
}

func (s *DatabaseService) loadLocalPiPermissions() error {
	if s.localPiPermissions == nil {
		s.localPiPermissions = make(map[string]localPiDatabasePermissions)
	}
	data, err := os.ReadFile(s.localPiPermissionsFile)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("read Pi database permissions: %w", err)
	}
	if err := json.Unmarshal(data, &s.localPiPermissions); err != nil {
		return fmt.Errorf("parse Pi database permissions: %w", err)
	}
	return nil
}

func (s *DatabaseService) persistLocalPiPermissionsLocked() error {
	if s.localPiPermissionsFile == "" && len(s.localPiPermissions) == 0 {
		return nil
	}
	if len(s.localPiPermissions) == 0 {
		if err := os.Remove(s.localPiPermissionsFile); err != nil && !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("remove Pi database permissions: %w", err)
		}
		return nil
	}
	data, err := json.MarshalIndent(s.localPiPermissions, "", "  ")
	if err != nil {
		return fmt.Errorf("encode Pi database permissions: %w", err)
	}
	if err := os.WriteFile(s.localPiPermissionsFile, data, 0600); err != nil {
		return fmt.Errorf("save Pi database permissions: %w", err)
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
	db, err := s.database(profile, password)
	if err != nil {
		return "", err
	}
	if err := db.PingContext(ctx); err != nil {
		return "", fmt.Errorf("connection failed: %w", err)
	}
	var version string
	versionQuery := "SELECT version()"
	if profile.Engine == "snowflake" {
		versionQuery = "SELECT CURRENT_VERSION()"
	}
	if err := db.QueryRowContext(ctx, versionQuery).Scan(&version); err != nil {
		return "", fmt.Errorf("connected, but could not read server version: %w", err)
	}
	return version, nil
}

func (s *DatabaseService) Databases(id, password string) ([]string, error) {
	profile, err := s.profile(id)
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	db, err := s.database(profile, password)
	if err != nil {
		return nil, err
	}
	databaseQuery := "SHOW DATABASES"
	if profile.Engine == "postgres" {
		databaseQuery = "SELECT datname FROM pg_database WHERE datistemplate = false AND has_database_privilege(datname, 'CONNECT') ORDER BY datname"
	}
	rows, err := db.QueryContext(ctx, databaseQuery)
	if err != nil {
		return nil, fmt.Errorf("list databases: %w", err)
	}
	defer rows.Close()
	columns, err := rows.Columns()
	if err != nil {
		return nil, fmt.Errorf("read database list columns: %w", err)
	}
	if len(columns) == 0 {
		return []string{}, nil
	}
	nameIndex := 0
	for i, column := range columns {
		if strings.EqualFold(column, "name") || strings.EqualFold(column, "database_name") || strings.EqualFold(column, "datname") {
			nameIndex = i
			break
		}
	}
	databases := make([]string, 0)
	for rows.Next() {
		values := make([]any, len(columns))
		destinations := make([]any, len(columns))
		for i := range values {
			destinations[i] = &values[i]
		}
		if err := rows.Scan(destinations...); err != nil {
			return nil, fmt.Errorf("read database list: %w", err)
		}
		name := values[nameIndex]
		switch value := name.(type) {
		case string:
			databases = append(databases, value)
		case []byte:
			databases = append(databases, string(value))
		default:
			databases = append(databases, fmt.Sprint(value))
		}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read database list: %w", err)
	}
	return databases, nil
}

func (s *DatabaseService) ExploreSchema(id, password, database string) ([]DatabaseSchema, error) {
	profile, err := s.profileForDatabase(id, database)
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Second)
	defer cancel()
	db, err := s.database(profile, password)
	if err != nil {
		return nil, err
	}

	rows, err := db.QueryContext(ctx, `
		SELECT schema_name
		FROM information_schema.schemata
		WHERE UPPER(schema_name) NOT IN ('INFORMATION_SCHEMA', 'PG_CATALOG', 'CRDB_INTERNAL')
		ORDER BY schema_name`)
	if err != nil {
		return nil, fmt.Errorf("list schemas: %w", err)
	}
	schemas := make([]DatabaseSchema, 0)
	indices := make(map[string]int)
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			rows.Close()
			return nil, fmt.Errorf("read schema list: %w", err)
		}
		indices[name] = len(schemas)
		schemas = append(schemas, DatabaseSchema{Name: name, Objects: []DatabaseObject{}})
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, fmt.Errorf("read schema list: %w", err)
	}
	rows.Close()

	rows, err = db.QueryContext(ctx, `
		SELECT t.table_schema, t.table_name, t.table_type,
		       c.column_name, c.data_type, c.is_nullable, c.column_default
		FROM information_schema.tables AS t
		LEFT JOIN information_schema.columns AS c
		  ON c.table_schema = t.table_schema AND c.table_name = t.table_name
		WHERE UPPER(t.table_schema) NOT IN ('INFORMATION_SCHEMA', 'PG_CATALOG', 'CRDB_INTERNAL')
		ORDER BY t.table_schema, t.table_name, c.ordinal_position`)
	if err != nil {
		return nil, fmt.Errorf("list tables and columns: %w", err)
	}
	defer rows.Close()
	objectIndices := make(map[string]int)
	for rows.Next() {
		var schemaName, objectName, tableType string
		var columnName, dataType, nullable, defaultValue sql.NullString
		if err := rows.Scan(&schemaName, &objectName, &tableType, &columnName, &dataType, &nullable, &defaultValue); err != nil {
			return nil, fmt.Errorf("read schema objects: %w", err)
		}
		schemaIndex, ok := indices[schemaName]
		if !ok {
			continue
		}
		key := schemaName + "\\x00" + objectName
		objectIndex, ok := objectIndices[key]
		if !ok {
			kind := "table"
			if strings.EqualFold(tableType, "VIEW") {
				kind = "view"
			} else if strings.Contains(strings.ToLower(tableType), "materialized") {
				kind = "materialized view"
			}
			objectIndex = len(schemas[schemaIndex].Objects)
			objectIndices[key] = objectIndex
			schemas[schemaIndex].Objects = append(schemas[schemaIndex].Objects, DatabaseObject{Name: objectName, Kind: kind, Columns: []DatabaseColumn{}})
		}
		if columnName.Valid {
			schemas[schemaIndex].Objects[objectIndex].Columns = append(schemas[schemaIndex].Objects[objectIndex].Columns, DatabaseColumn{
				Name: columnName.String, DataType: dataType.String,
				Nullable: nullable.String == "YES", Default: defaultValue.String,
			})
		}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read schema objects: %w", err)
	}
	return schemas, nil
}

func (s *DatabaseService) RunQuery(id, password, database, query string) (QueryResult, error) {
	profile, err := s.profileForDatabase(id, database)
	if err != nil {
		return QueryResult{}, err
	}
	query = strings.TrimSpace(query)
	if query == "" {
		return QueryResult{}, errors.New("enter a SQL statement first")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	db, err := s.database(profile, password)
	if err != nil {
		return QueryResult{}, err
	}

	started := time.Now()
	result := QueryResult{Rows: make([][]any, 0), CommandTag: "query"}
	for _, statement := range splitSQLStatements(query) {
		rows, err := db.QueryContext(ctx, statement)
		if err != nil {
			return QueryResult{}, fmt.Errorf("query failed: %w", err)
		}
		columns, err := rows.Columns()
		if err != nil {
			rows.Close()
			return QueryResult{}, fmt.Errorf("read result columns: %w", err)
		}
		result.Columns = columns
		result.Rows = make([][]any, 0)
		for rows.Next() {
			values := make([]any, len(columns))
			destinations := make([]any, len(columns))
			for i := range values {
				destinations[i] = &values[i]
			}
			if err := rows.Scan(destinations...); err != nil {
				rows.Close()
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
			rows.Close()
			return QueryResult{}, fmt.Errorf("read query result: %w", err)
		}
		if err := rows.Close(); err != nil {
			return QueryResult{}, fmt.Errorf("close query result: %w", err)
		}
	}
	result.RowCount = int64(len(result.Rows))
	result.DurationMs = time.Since(started).Milliseconds()
	return result, nil
}

// splitSQLStatements separates on semicolons outside strings, quoted identifiers,
// comments, and PostgreSQL dollar-quoted strings.
func splitSQLStatements(query string) []string {
	var statements []string
	start := 0
	for i := 0; i < len(query); {
		switch {
		case i+1 < len(query) && query[i:i+2] == "--":
			for i < len(query) && query[i] != '\n' {
				i++
			}
		case i+1 < len(query) && query[i:i+2] == "/*":
			i += 2
			depth := 1
			for i < len(query) && depth > 0 {
				if i+1 < len(query) && query[i:i+2] == "/*" {
					depth++
					i += 2
				} else if i+1 < len(query) && query[i:i+2] == "*/" {
					depth--
					i += 2
				} else {
					i++
				}
			}
		case query[i] == '\'' || query[i] == '"' || query[i] == '`':
			quote := query[i]
			i++
			for i < len(query) {
				if query[i] == quote {
					if i+1 < len(query) && query[i+1] == quote {
						i += 2
						continue
					}
					i++
					break
				}
				if quote == '\'' && query[i] == '\\' && i+1 < len(query) {
					i += 2
				} else {
					i++
				}
			}
		case query[i] == '$':
			delim := ""
			for j := i + 1; j < len(query) && (query[j] == '_' || query[j] >= 'a' && query[j] <= 'z' || query[j] >= 'A' && query[j] <= 'Z' || query[j] >= '0' && query[j] <= '9'); j++ {
				if query[j] == '$' {
					delim = query[i : j+1]
					break
				}
			}
			if delim != "" {
				if end := strings.Index(query[i+len(delim):], delim); end >= 0 {
					i += len(delim) + end + len(delim)
				} else {
					i = len(query)
				}
			} else {
				i++
			}
		case query[i] == ';':
			if statement := strings.TrimSpace(query[start:i]); statement != "" {
				statements = append(statements, statement)
			}
			i++
			start = i
		default:
			i++
		}
	}
	if statement := strings.TrimSpace(query[start:]); statement != "" {
		statements = append(statements, statement)
	}
	return statements
}

func (s *DatabaseService) closeProfilePoolsLocked(id string) {
	prefix := id + "\x00"
	for key, pool := range s.pools {
		if strings.HasPrefix(key, prefix) {
			_ = pool.db.Close()
			delete(s.pools, key)
		}
	}
}

func (s *DatabaseService) database(profile ConnectionProfile, password string) (*sql.DB, error) {
	key := profile.ID + "\x00" + profile.Database
	fingerprint := sha256.Sum256([]byte(strings.Join([]string{
		profile.Engine, profile.Host, profile.Port, profile.Database, profile.Schema, profile.Warehouse, profile.Role, profile.Username, profile.SSLMode, password,
	}, "\x00")))

	s.mu.Lock()
	defer s.mu.Unlock()
	if pool, ok := s.pools[key]; ok && pool.fingerprint == fingerprint {
		return pool.db, nil
	}
	db, err := openDatabase(profile, password)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(5)
	db.SetMaxIdleConns(2)
	db.SetConnMaxIdleTime(5 * time.Minute)
	db.SetConnMaxLifetime(30 * time.Minute)
	if old, ok := s.pools[key]; ok {
		_ = old.db.Close()
	}
	s.pools[key] = databasePool{db: db, fingerprint: fingerprint}
	return db, nil
}

// Close releases all cached database pools when the desktop application exits.
func (s *DatabaseService) Close() error {
	s.stopAllPiAssistants()
	s.mu.Lock()
	defer s.mu.Unlock()
	var firstError error
	for key, pool := range s.pools {
		if err := pool.db.Close(); err != nil && firstError == nil {
			firstError = err
		}
		delete(s.pools, key)
	}
	return firstError
}

func (s *DatabaseService) profile(id string) (ConnectionProfile, error) {
	return s.profileForDatabase(id, "")
}

func (s *DatabaseService) profileForDatabase(id, database string) (ConnectionProfile, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, profile := range s.profiles {
		if profile.ID == id {
			if database != "" {
				profile.Database = database
			}
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
		if permissions, ok := s.localPiPermissions[id]; ok {
			profile.AllowPiDatabaseAccess = permissions.AllowPiDatabaseAccess
			profile.AllowPiDatabaseWrite = permissions.AllowPiDatabaseWrite
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

func (s *DatabaseService) loadLocalProjectDirectories() error {
	data, err := os.ReadFile(s.localProjectDirsFile)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("read project directories: %w", err)
	}
	if err := json.Unmarshal(data, &s.localProjectDirs); err != nil {
		return fmt.Errorf("parse project directories: %w", err)
	}
	for i := range s.profiles {
		if directory, ok := s.localProjectDirs[s.profiles[i].ID]; ok && s.localIDs[s.profiles[i].ID] {
			s.profiles[i].ProjectDirectory = directory
		}
	}
	return nil
}

func (s *DatabaseService) persistLocalProjectDirectoriesLocked() error {
	data, err := json.MarshalIndent(s.localProjectDirs, "", "  ")
	if err != nil {
		return fmt.Errorf("encode project directories: %w", err)
	}
	if err := os.WriteFile(s.localProjectDirsFile, data, 0600); err != nil {
		return fmt.Errorf("save project directories: %w", err)
	}
	return nil
}

func openDatabase(profile ConnectionProfile, password string) (*sql.DB, error) {
	if profile.Engine == "snowflake" {
		config := &gosnowflake.Config{
			Account: profile.Host, User: profile.Username, Password: password,
			Database: profile.Database, Schema: profile.Schema,
			Warehouse: profile.Warehouse, Role: profile.Role,
		}
		dsn, err := gosnowflake.DSN(config)
		if err != nil {
			return nil, fmt.Errorf("invalid Snowflake settings: %w", err)
		}
		return sql.Open("snowflake", dsn)
	}
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
