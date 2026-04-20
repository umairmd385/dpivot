package compose_test

import (
	"testing"

	"github.com/dpivot/dpivot/internal/compose"
)

// ── IsDatabase ────────────────────────────────────────────────────────────────

func TestIsDatabase_KnownImages(t *testing.T) {
	cases := []struct {
		image string
		want  bool
	}{
		// Exact names
		{"postgres", true},
		{"mysql", true},
		{"redis", true},
		{"mariadb", true},
		{"mongo", true},
		{"mongodb", true},
		{"elasticsearch", true},
		{"opensearch", true},
		{"cassandra", true},
		{"couchdb", true},
		{"influxdb", true},
		{"rabbitmq", true},
		{"kafka", true},
		{"zookeeper", true},
		{"mssql", true},
		{"clickhouse", true},
		{"minio", true},
		{"vault", true},
		// With tags
		{"postgres:16", true},
		{"mysql:8.0", true},
		{"redis:7-alpine", true},
		// With registry prefix
		{"docker.io/library/postgres:16", true},
		{"gcr.io/my-project/postgres:latest", true},
		// With org prefix
		{"bitnami/redis:7", true},
		{"bitnami/mysql:8.0", true},
		// App images (not databases)
		{"myapp:latest", false},
		{"node:20-alpine", false},
		{"nginx:alpine", false},
		{"golang:1.22", false},
		{"python:3.12", false},
		{"ubuntu:22.04", false},
		{"", false},
	}
	for _, tc := range cases {
		got := compose.IsDatabase(tc.image)
		if got != tc.want {
			t.Errorf("IsDatabase(%q) = %v, want %v", tc.image, got, tc.want)
		}
	}
}

func TestIsDatabase_CaseInsensitive(t *testing.T) {
	cases := []string{"POSTGRES", "Mysql", "REDIS", "MongoDB"}
	for _, img := range cases {
		if !compose.IsDatabase(img) {
			t.Errorf("IsDatabase(%q): want true (case-insensitive match)", img)
		}
	}
}

// ── ShouldProxy ───────────────────────────────────────────────────────────────

func TestShouldProxy(t *testing.T) {
	cases := []struct {
		name string
		svc  compose.Service
		want bool
	}{
		{
			name: "app with ports",
			svc:  compose.Service{Image: "myapp:latest", Ports: []string{"3000:3000"}},
			want: true,
		},
		{
			name: "app with no ports",
			svc:  compose.Service{Image: "myapp:latest"},
			want: false,
		},
		{
			name: "database with ports",
			svc:  compose.Service{Image: "postgres:16", Ports: []string{"5432:5432"}},
			want: false,
		},
		{
			name: "explicit skip with ports",
			svc:  compose.Service{Image: "myapp:latest", Ports: []string{"3000:3000"}, XDpivot: compose.XDpivotConfig{Skip: true}},
			want: false,
		},
		{
			name: "explicit skip on database",
			svc:  compose.Service{Image: "mysql:8.0", Ports: []string{"3306:3306"}, XDpivot: compose.XDpivotConfig{Skip: true}},
			want: false,
		},
		{
			name: "worker with no ports",
			svc:  compose.Service{Image: "myapp:latest"},
			want: false,
		},
	}
	for _, tc := range cases {
		got := compose.ShouldProxy(tc.svc)
		if got != tc.want {
			t.Errorf("ShouldProxy(%s) = %v, want %v", tc.name, got, tc.want)
		}
	}
}

func TestDatabaseImages_NotEmpty(t *testing.T) {
	imgs := compose.DatabaseImages()
	if len(imgs) == 0 {
		t.Fatal("DatabaseImages() returned empty slice")
	}
}
