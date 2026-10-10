package main

import "testing"

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
