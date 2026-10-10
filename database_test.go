package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSameDatabaseConnectionSettingsIgnoresPiPermissions(t *testing.T) {
	original := ConnectionProfile{
		ID: "profile", Engine: "cockroach", Host: "db.example", Port: "26257",
		Database: "app", Username: "user", SSLMode: "verify-full",
	}
	updated := original
	updated.AllowPiDatabaseAccess = true
	updated.AllowPiDatabaseWrite = true
	updated.Name = "Renamed profile"
	if !sameDatabaseConnectionSettings(original, updated) {
		t.Fatal("Pi permissions/name changes should retain the database pool")
	}
	updated.Host = "other.example"
	if sameDatabaseConnectionSettings(original, updated) {
		t.Fatal("host changes must invalidate the database pool")
	}
}

func TestLocalPiPermissionsPersistAcrossServiceRestart(t *testing.T) {
	dir := t.TempDir()
	localFile := filepath.Join(dir, "connections.local.json")
	permissionsFile := filepath.Join(dir, "pi-permissions.json")
	connectionURI := "postgresql://tester:local-secret@localhost:26257/sample?sslmode=disable"
	if err := os.WriteFile(localFile, []byte(`{"connections":[{"name":"Local DB","dbURI":"`+connectionURI+`"}]}`), 0600); err != nil {
		t.Fatal(err)
	}
	newService := func() *DatabaseService {
		t.Helper()
		service := &DatabaseService{
			file:                   filepath.Join(dir, "connections.json"),
			localPiPermissionsFile: permissionsFile,
			localPiPermissions:     make(map[string]localPiDatabasePermissions),
			localPasswords:         make(map[string]string),
			localIDs:               make(map[string]bool),
			pools:                  make(map[string]databasePool),
		}
		if err := service.loadLocalPiPermissions(); err != nil {
			t.Fatal(err)
		}
		if err := service.loadLocalConnections(localFile); err != nil {
			t.Fatal(err)
		}
		return service
	}

	service := newService()
	profile := service.Profiles()[0]
	profile.AllowPiDatabaseAccess = true
	profile.AllowPiDatabaseWrite = true
	if _, err := service.SaveProfile(profile); err != nil {
		t.Fatal(err)
	}

	restarted := newService()
	loaded := restarted.Profiles()[0]
	if !loaded.AllowPiDatabaseAccess || !loaded.AllowPiDatabaseWrite {
		t.Fatalf("Pi permissions did not persist: access=%t write=%t", loaded.AllowPiDatabaseAccess, loaded.AllowPiDatabaseWrite)
	}
	data, err := os.ReadFile(permissionsFile)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "local-secret") || strings.Contains(string(data), connectionURI) {
		t.Fatal("persisted Pi permissions must not contain connection credentials")
	}
}
