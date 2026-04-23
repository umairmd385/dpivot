package compose_test

import (
	"strings"
	"testing"

	"github.com/dpivot/dpivot/internal/compose"
)

const generatorInput = `
version: "3.9"
services:
  api:
    image: myapp:latest
    ports:
      - "3000:3000"
    environment:
      PORT: "3000"
  db:
    image: postgres:16
    ports:
      - "5432:5432"
  worker:
    image: myapp:latest
`

func parse(t *testing.T, y string) *compose.ComposeFile {
	t.Helper()
	cf, err := compose.ParseBytes([]byte(y))
	if err != nil {
		t.Fatalf("ParseBytes: %v", err)
	}
	return cf
}

func TestGenerate_Basic(t *testing.T) {
	cf := parse(t, generatorInput)
	_, sum, err := compose.Generate(cf)
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	if len(sum.Proxied) != 1 || sum.Proxied[0] != "api" {
		t.Errorf("Proxied = %v, want [api]", sum.Proxied)
	}
	if len(sum.Skipped) != 1 || sum.Skipped[0] != "db" {
		t.Errorf("Skipped = %v, want [db]", sum.Skipped)
	}
	if len(sum.PassThru) != 1 || sum.PassThru[0] != "worker" {
		t.Errorf("PassThru = %v, want [worker]", sum.PassThru)
	}
}

func TestGenerate_ProxyServiceCreated(t *testing.T) {
	cf := parse(t, generatorInput)
	out, _, _ := compose.Generate(cf)
	if _, ok := out.Services["dpivot-proxy-api"]; !ok {
		t.Error("dpivot-proxy-api service should be in generated output")
	}
}

func TestGenerate_BackingServiceHasNoHostPorts(t *testing.T) {
	cf := parse(t, generatorInput)
	out, _, _ := compose.Generate(cf)
	api := out.Services["api"]
	if len(api.Ports) != 0 {
		t.Errorf("backing service should have no host ports, got %v", api.Ports)
	}
}

func TestGenerate_BackingServiceHasExpose(t *testing.T) {
	cf := parse(t, generatorInput)
	out, _, _ := compose.Generate(cf)
	api := out.Services["api"]
	found := false
	for _, e := range api.Expose {
		if e == "3000" {
			found = true
		}
	}
	if !found {
		t.Errorf("backing api.Expose should contain 3000, got %v", api.Expose)
	}
}

func TestGenerate_BackingServiceJoinsMesh(t *testing.T) {
	cf := parse(t, generatorInput)
	out, _, _ := compose.Generate(cf)
	api := out.Services["api"]
	found := false
	for _, n := range api.Networks {
		if n == "dpivot_mesh" {
			found = true
		}
	}
	if !found {
		t.Errorf("backing api.Networks should contain dpivot_mesh, got %v", api.Networks)
	}
}

func TestGenerate_ProxyOwnsHostPorts(t *testing.T) {
	cf := parse(t, generatorInput)
	out, _, _ := compose.Generate(cf)
	proxyService := out.Services["dpivot-proxy-api"]
	if len(proxyService.Ports) == 0 {
		t.Error("proxy service should own the host port")
	}
}

func TestGenerate_ProxyHasDpivotBinds(t *testing.T) {
	cf := parse(t, generatorInput)
	out, _, _ := compose.Generate(cf)
	proxy := out.Services["dpivot-proxy-api"]
	if proxy.Environment["DPIVOT_BINDS"] == "" {
		t.Error("proxy service should have DPIVOT_BINDS set")
	}
}

func TestGenerate_MeshNetworkCreated(t *testing.T) {
	cf := parse(t, generatorInput)
	out, _, _ := compose.Generate(cf)
	if _, ok := out.Networks["dpivot_mesh"]; !ok {
		t.Error("dpivot_mesh network should be in generated output")
	}
}

func TestGenerate_DatabaseNotProxied(t *testing.T) {
	cf := parse(t, generatorInput)
	out, _, _ := compose.Generate(cf)
	if _, ok := out.Services["dpivot-proxy-db"]; ok {
		t.Error("dpivot-proxy-db should NOT be created for database service")
	}
	db := out.Services["db"]
	if len(db.Ports) == 0 {
		t.Error("database service should keep its original ports")
	}
}

func TestGenerate_WorkerPassThrough(t *testing.T) {
	cf := parse(t, generatorInput)
	out, _, _ := compose.Generate(cf)
	if _, ok := out.Services["dpivot-proxy-worker"]; ok {
		t.Error("dpivot-proxy-worker should NOT be created for worker (no ports)")
	}
}

func TestGenerate_SkipOverride(t *testing.T) {
	y := `
version: "3.9"
services:
  app:
    image: myapp:latest
    ports: ["8080:8080"]
    x-dpivot:
      skip: true
`
	cf := parse(t, y)
	out, sum, _ := compose.Generate(cf)
	if _, ok := out.Services["dpivot-proxy-app"]; ok {
		t.Error("proxy should not be created when x-dpivot.skip is true")
	}
	if len(sum.PassThru) != 1 {
		t.Errorf("sum.PassThru = %v, want [app]", sum.PassThru)
	}
}

func TestGenerate_OriginalNotModified(t *testing.T) {
	cf := parse(t, generatorInput)
	origPortCount := len(cf.Services["api"].Ports)
	compose.Generate(cf) //nolint:errcheck
	if len(cf.Services["api"].Ports) != origPortCount {
		t.Error("Generate must not modify the input ComposeFile")
	}
}

func TestGenerate_BackingServiceLabels(t *testing.T) {
	cf := parse(t, generatorInput)
	out, _, _ := compose.Generate(cf)
	api := out.Services["api"]
	labels, ok := api.RawFields["labels"].(map[string]interface{})
	if !ok {
		t.Fatal("backing service should have labels in RawFields")
	}
	if labels["dpivot.managed"] != "true" {
		t.Errorf("dpivot.managed = %v, want true", labels["dpivot.managed"])
	}
}

func TestGenerate_MultiPort(t *testing.T) {
	y := `
version: "3.9"
services:
  frontend:
    image: nginx:alpine
    ports:
      - "80:80"
      - "443:443"
`
	cf := parse(t, y)
	out, _, err := compose.Generate(cf)
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	proxy := out.Services["dpivot-proxy-frontend"]
	// 2 traffic ports (80:80, 443:443) + 1 control port (firstHostPort+6900 → 9900)
	if len(proxy.Ports) != 3 {
		t.Errorf("multi-port proxy should have 3 ports (2 traffic + 1 control), got %d: %v", len(proxy.Ports), proxy.Ports)
	}
	binds := proxy.Environment["DPIVOT_BINDS"]
	if !strings.Contains(binds, "80") || !strings.Contains(binds, "443") {
		t.Errorf("DPIVOT_BINDS = %q, want both port 80 and 443", binds)
	}
	// Control port should be 80+6900=6980 mapped to 9900
	controlPort := proxy.Ports[len(proxy.Ports)-1]
	if controlPort != "6980:9900" {
		t.Errorf("control port mapping = %q, want 6980:9900", controlPort)
	}
}

func TestGenerate_NilInput_ReturnsError(t *testing.T) {
	_, _, err := compose.Generate(nil)
	if err == nil {
		t.Fatal("want error for nil input, got nil")
	}
}
