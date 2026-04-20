package proxy_test

import (
	"fmt"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/dpivot/dpivot/internal/proxy"
)

func newTestRouter(t *testing.T, backendCount int) (*proxy.Registry, *proxy.Router) {
	t.Helper()
	reg := proxy.NewRegistry()
	for i := 0; i < backendCount; i++ {
		err := reg.Add(proxy.Backend{
			ID:   fmt.Sprintf("b%d", i),
			Addr: fmt.Sprintf("10.0.0.%d:8080", i+1),
		})
		if err != nil {
			t.Fatalf("Add b%d: %v", i, err)
		}
	}
	return reg, proxy.NewRouter(reg)
}

func TestRouter_RoundRobin(t *testing.T) {
	_, router := newTestRouter(t, 2)
	// Make 20 calls (10 full rounds) and count backend selections.
	counts := map[string]int{}
	for i := 0; i < 20; i++ {
		b, err := router.Next()
		if err != nil {
			t.Fatalf("Next() call %d: %v", i, err)
		}
		counts[b.ID]++
	}
	// With 2 backends and 20 calls each backend should be chosen 10 times.
	for id, n := range counts {
		if n != 10 {
			t.Errorf("backend %q selected %d/20 times, want 10 (equal distribution)", id, n)
		}
	}
}

func TestRouter_SingleBackend(t *testing.T) {
	_, router := newTestRouter(t, 1)
	b0, _ := router.Next()
	b1, _ := router.Next()
	if b0.ID != b1.ID {
		t.Errorf("single-backend: expected same ID every call, got %s and %s", b0.ID, b1.ID)
	}
}

func TestRouter_NoBackends(t *testing.T) {
	_, router := newTestRouter(t, 0)
	_, err := router.Next()
	if err == nil {
		t.Fatal("want error when no backends, got nil")
	}
}

func TestRouter_SkipsDrainingBackends(t *testing.T) {
	reg, router := newTestRouter(t, 2)
	_ = reg.SetDraining("b0")

	for i := 0; i < 10; i++ {
		b, err := router.Next()
		if err != nil {
			t.Fatalf("Next: %v", err)
		}
		if b.ID == "b0" {
			t.Errorf("draining backend b0 was selected at iteration %d", i)
		}
	}
}

func TestRouter_AllDraining_ReturnsError(t *testing.T) {
	reg, router := newTestRouter(t, 2)
	_ = reg.SetDraining("b0")
	_ = reg.SetDraining("b1")
	_, err := router.Next()
	if err == nil {
		t.Fatal("want error when all backends draining, got nil")
	}
}

func TestRouter_ConcurrentNext(t *testing.T) {
	_, router := newTestRouter(t, 4)
	var errCount int64
	var wg sync.WaitGroup
	for i := 0; i < 500; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := router.Next(); err != nil {
				atomic.AddInt64(&errCount, 1)
			}
		}()
	}
	wg.Wait()
	if errCount > 0 {
		t.Errorf("got %d errors from concurrent Next()", errCount)
	}
}
