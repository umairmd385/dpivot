package rollout

import "testing"

func TestPickBackendPortPrefersDPivotBackendEnv(t *testing.T) {
	t.Parallel()

	port, err := pickBackendPort(
		[]string{"443/tcp", "8080/tcp", "3000/tcp"},
		[]string{"FOO=bar", "DPIVOT_BACKEND=api:3000"},
	)
	if err != nil {
		t.Fatalf("pickBackendPort returned error: %v", err)
	}
	if port != "3000" {
		t.Fatalf("expected port 3000, got %s", port)
	}
}

func TestPickBackendPortFallsBackToSmallestExposedPort(t *testing.T) {
	t.Parallel()

	port, err := pickBackendPort([]string{"443/tcp", "8080/tcp", "3000/tcp"}, nil)
	if err != nil {
		t.Fatalf("pickBackendPort returned error: %v", err)
	}
	if port != "443" {
		t.Fatalf("expected port 443, got %s", port)
	}
}

func TestPickBackendPortDefaultsTo80WhenNoPorts(t *testing.T) {
	t.Parallel()

	port, err := pickBackendPort(nil, nil)
	if err != nil {
		t.Fatalf("pickBackendPort returned error: %v", err)
	}
	if port != "80" {
		t.Fatalf("expected default port 80, got %s", port)
	}
}
