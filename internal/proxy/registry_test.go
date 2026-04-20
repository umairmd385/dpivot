package proxy_test

import (
	"sync"
	"testing"

	"github.com/dpivot/dpivot/internal/proxy"
)

func TestRegistry_Add_Valid(t *testing.T) {
	reg := proxy.NewRegistry()
	if err := reg.Add(proxy.Backend{ID: "b1", Addr: "10.0.0.1:8080"}); err != nil {
		t.Fatalf("Add: %v", err)
	}
	if reg.Len() != 1 {
		t.Errorf("Len = %d, want 1", reg.Len())
	}
}

func TestRegistry_Add_DuplicateID(t *testing.T) {
	reg := proxy.NewRegistry()
	reg.Add(proxy.Backend{ID: "b1", Addr: "10.0.0.1:8080"})   //nolint:errcheck
	err := reg.Add(proxy.Backend{ID: "b1", Addr: "10.0.0.2:8080"})
	if err == nil {
		t.Fatal("want error for duplicate ID, got nil")
	}
}

func TestRegistry_Add_EmptyID(t *testing.T) {
	reg := proxy.NewRegistry()
	err := reg.Add(proxy.Backend{ID: "", Addr: "10.0.0.1:8080"})
	if err == nil {
		t.Fatal("want error for empty ID")
	}
}

func TestRegistry_Add_EmptyAddr(t *testing.T) {
	reg := proxy.NewRegistry()
	err := reg.Add(proxy.Backend{ID: "b1", Addr: ""})
	if err == nil {
		t.Fatal("want error for empty Addr")
	}
}

func TestRegistry_Remove_Existing(t *testing.T) {
	reg := proxy.NewRegistry()
	reg.Add(proxy.Backend{ID: "b1", Addr: "10.0.0.1:8080"}) //nolint:errcheck
	if err := reg.Remove("b1"); err != nil {
		t.Fatalf("Remove: %v", err)
	}
	if reg.Len() != 0 {
		t.Errorf("Len = %d, want 0 after remove", reg.Len())
	}
}

func TestRegistry_Remove_Unknown(t *testing.T) {
	reg := proxy.NewRegistry()
	err := reg.Remove("nonexistent")
	if err == nil {
		t.Fatal("want error when removing unknown ID")
	}
}

func TestRegistry_SetDraining(t *testing.T) {
	reg := proxy.NewRegistry()
	reg.Add(proxy.Backend{ID: "b1", Addr: "10.0.0.1:8080"}) //nolint:errcheck
	if err := reg.SetDraining("b1"); err != nil {
		t.Fatalf("SetDraining: %v", err)
	}
	b, ok := reg.Get("b1")
	if !ok || !b.Draining {
		t.Error("backend should be marked draining")
	}
}

func TestRegistry_SetDraining_Unknown(t *testing.T) {
	reg := proxy.NewRegistry()
	if err := reg.SetDraining("ghost"); err == nil {
		t.Fatal("want error for unknown ID")
	}
}

func TestRegistry_Active_ExcludesDraining(t *testing.T) {
	reg := proxy.NewRegistry()
	reg.Add(proxy.Backend{ID: "b1", Addr: "10.0.0.1:8080"}) //nolint:errcheck
	reg.Add(proxy.Backend{ID: "b2", Addr: "10.0.0.2:8080"}) //nolint:errcheck
	reg.SetDraining("b1")                                    //nolint:errcheck
	active := reg.Active()
	if len(active) != 1 || active[0].ID != "b2" {
		t.Errorf("Active() = %v, want only b2", active)
	}
}

func TestRegistry_Backends_IncludesDraining(t *testing.T) {
	reg := proxy.NewRegistry()
	reg.Add(proxy.Backend{ID: "b1", Addr: "10.0.0.1:8080"}) //nolint:errcheck
	reg.Add(proxy.Backend{ID: "b2", Addr: "10.0.0.2:8080"}) //nolint:errcheck
	reg.SetDraining("b1")                                    //nolint:errcheck
	all := reg.Backends()
	if len(all) != 2 {
		t.Errorf("Backends() = %d, want 2", len(all))
	}
}

func TestRegistry_Get_NotFound(t *testing.T) {
	reg := proxy.NewRegistry()
	_, ok := reg.Get("missing")
	if ok {
		t.Error("Get should return false for unknown ID")
	}
}

func TestRegistry_Requests_Incremented(t *testing.T) {
	reg := proxy.NewRegistry()
	reg.Add(proxy.Backend{ID: "b1", Addr: "10.0.0.1:8080"}) //nolint:errcheck
	b, _ := reg.Get("b1")
	initial := b.Requests()
	// IncrRequests is called via the internal path; simulate via backend method.
	b.IncrRequests()
	if b.Requests() != initial+1 {
		t.Error("IncrRequests did not increment counter")
	}
}

func TestRegistry_ConcurrentAddRemove(t *testing.T) {
	reg := proxy.NewRegistry()
	var wg sync.WaitGroup
	for i := 0; i < 100; i++ {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			id := string(rune('a'+n%26)) + string(rune('0'+n%10))
			_ = reg.Add(proxy.Backend{ID: id, Addr: "10.0.0.1:8080"})
			_ = reg.Remove(id)
		}(i)
	}
	wg.Wait()
}
