package compose_test

import (
	"strings"
	"testing"

	"github.com/dpivot/dpivot/internal/compose"
)

const basicYAML = `
version: "3.9"
services:
  api:
    image: myapp:latest
    ports:
      - "3000:3000"
    environment:
      PORT: "3000"
    healthcheck:
      test: ["CMD", "wget", "-qO-", "http://localhost:3000/health"]
      interval: 10s
  db:
    image: postgres:16
    ports:
      - "5432:5432"
    environment:
      POSTGRES_PASSWORD: secret
  worker:
    image: myapp:latest
    environment:
      QUEUE: jobs
`

func TestParseBytes_Basic(t *testing.T) {
	cf, err := compose.ParseBytes([]byte(basicYAML))
	if err != nil {
		t.Fatalf("ParseBytes: %v", err)
	}
	if len(cf.Services) != 3 {
		t.Fatalf("want 3 services, got %d", len(cf.Services))
	}
}

func TestParseBytes_Ports(t *testing.T) {
	cf, err := compose.ParseBytes([]byte(basicYAML))
	if err != nil {
		t.Fatal(err)
	}
	api := cf.Services["api"]
	if len(api.Ports) != 1 || api.Ports[0] != "3000:3000" {
		t.Errorf("api.Ports: want [3000:3000], got %v", api.Ports)
	}
}

func TestParseBytes_EnvironmentMap(t *testing.T) {
	cf, err := compose.ParseBytes([]byte(basicYAML))
	if err != nil {
		t.Fatal(err)
	}
	if v := cf.Services["api"].Environment["PORT"]; v != "3000" {
		t.Errorf("api.Environment[PORT] = %q, want %q", v, "3000")
	}
}

func TestParseBytes_EnvironmentList(t *testing.T) {
	yaml := `
version: "3.9"
services:
  app:
    image: myapp
    environment:
      - KEY=value
      - OTHERKEY=othervalue
`
	cf, err := compose.ParseBytes([]byte(yaml))
	if err != nil {
		t.Fatal(err)
	}
	env := cf.Services["app"].Environment
	if env["KEY"] != "value" {
		t.Errorf("KEY = %q, want %q", env["KEY"], "value")
	}
	if env["OTHERKEY"] != "othervalue" {
		t.Errorf("OTHERKEY = %q, want %q", env["OTHERKEY"], "othervalue")
	}
}

func TestParseBytes_Image(t *testing.T) {
	cf, err := compose.ParseBytes([]byte(basicYAML))
	if err != nil {
		t.Fatal(err)
	}
	if cf.Services["db"].Image != "postgres:16" {
		t.Errorf("db.Image = %q, want postgres:16", cf.Services["db"].Image)
	}
}

func TestParseBytes_RawFieldsPreserved(t *testing.T) {
	cf, err := compose.ParseBytes([]byte(basicYAML))
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := cf.Services["api"].RawFields["healthcheck"]; !ok {
		t.Error("healthcheck should be preserved in RawFields")
	}
}

func TestParseBytes_XDpivotSkip(t *testing.T) {
	y := `
version: "3.9"
services:
  app:
    image: myapp
    ports:
      - "3000:3000"
    x-dpivot:
      skip: true
`
	cf, err := compose.ParseBytes([]byte(y))
	if err != nil {
		t.Fatal(err)
	}
	if !cf.Services["app"].XDpivot.Skip {
		t.Error("x-dpivot.skip should be true")
	}
}

func TestParseBytes_XDpivotNotInRawFields(t *testing.T) {
	y := `
version: "3.9"
services:
  app:
    image: myapp
    x-dpivot:
      skip: true
`
	cf, err := compose.ParseBytes([]byte(y))
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := cf.Services["app"].RawFields["x-dpivot"]; ok {
		t.Error("x-dpivot should NOT appear in RawFields (it must be stripped)")
	}
}

func TestParseBytes_EmptyFile(t *testing.T) {
	_, err := compose.ParseBytes([]byte(""))
	if err == nil {
		t.Fatal("want error for empty file, got nil")
	}
}

func TestParseBytes_NoServices(t *testing.T) {
	_, err := compose.ParseBytes([]byte("version: \"3.9\"\n"))
	if err == nil {
		t.Fatal("want error when no services defined")
	}
	if !strings.Contains(err.Error(), "no services") {
		t.Errorf("unexpected error: %v", err)
	}
}

func TestParseBytes_InvalidYAML(t *testing.T) {
	_, err := compose.ParseBytes([]byte("{{not yaml"))
	if err == nil {
		t.Fatal("want error for invalid YAML")
	}
}

func TestParseBytes_MultiPort(t *testing.T) {
	y := `
version: "3.9"
services:
  frontend:
    image: nginx
    ports:
      - "80:80"
      - "443:443"
`
	cf, err := compose.ParseBytes([]byte(y))
	if err != nil {
		t.Fatal(err)
	}
	ports := cf.Services["frontend"].Ports
	if len(ports) != 2 {
		t.Fatalf("want 2 ports, got %d", len(ports))
	}
}

func TestParseBytes_Version(t *testing.T) {
	cf, err := compose.ParseBytes([]byte(basicYAML))
	if err != nil {
		t.Fatal(err)
	}
	if cf.Version != "3.9" {
		t.Errorf("Version = %q, want 3.9", cf.Version)
	}
}
