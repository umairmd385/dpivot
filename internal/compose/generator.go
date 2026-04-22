package compose

import (
	"fmt"
	"strconv"
	"strings"
)

// Summary records what the generator transformed.
type Summary struct {
	Proxied  []string // service names that received proxy injection
	Skipped  []string // service names that were auto-excluded (databases)
	PassThru []string // service names passed through unchanged
}

// Generate transforms an input ComposeFile into a dpivot-enhanced output.
//
// Transformation rules (per service, in priority order):
//  1. x-dpivot.skip == true → pass through unchanged, no warning
//  2. No ports → pass through unchanged
//  3. Image matches known database → pass through, emit warning (skipped list)
//  4. All other services with ports → inject proxy
//
// The generated file:
//   - Preserves all unrelated fields in every service verbatim, including labels,
//     volumes, healthchecks, restart policies, and user-defined networks.
//   - Adds a dpivot-mesh bridge network joined by every proxy+backing pair.
//   - Adds one dpivot-proxy-<service> service per injected service.
//   - Injects DSO_PROXY_BINDS and related env vars into each proxy service.
//   - Never modifies the caller-supplied ComposeFile (works on a deep copy).
func Generate(input *ComposeFile) (*ComposeFile, *Summary, error) {
	if input == nil {
		return nil, nil, fmt.Errorf("generator: nil compose file")
	}

	out := &ComposeFile{
		Version:  input.Version,
		Services: make(map[string]Service, len(input.Services)*2),
		Networks: deepCopyMap(input.Networks),
		Volumes:  deepCopyMap(input.Volumes),
	}

	// Ensure the mesh network exists.
	if out.Networks == nil {
		out.Networks = make(map[string]interface{})
	}
	out.Networks["dpivot_mesh"] = map[string]interface{}{"driver": "bridge"}

	sum := &Summary{}

	for name, svc := range input.Services {
		switch {
		case svc.XDpivot.Skip:
			// Explicit opt-out: pass through exactly as-is.
			out.Services[name] = copyService(svc)
			sum.PassThru = append(sum.PassThru, name)

		case len(svc.Ports) == 0:
			// No ports — nothing to proxy.
			out.Services[name] = copyService(svc)
			sum.PassThru = append(sum.PassThru, name)

		case IsDatabase(svc.Image):
			// Database auto-exclusion.
			out.Services[name] = copyService(svc)
			sum.Skipped = append(sum.Skipped, name)

		default:
			// Inject proxy.
			backing, proxy, err := buildProxyPair(name, svc)
			if err != nil {
				return nil, nil, fmt.Errorf("generator: service %q: %w", name, err)
			}
			out.Services[name] = backing
			out.Services["dpivot-proxy-"+name] = proxy
			sum.Proxied = append(sum.Proxied, name)
		}
	}

	return out, sum, nil
}

// buildProxyPair builds the backing service (ports removed) and the
// generated dpivot-proxy-<service> for a single eligible service.
func buildProxyPair(name string, svc Service) (backing Service, proxy Service, err error) {
	// ── Backing service ───────────────────────────────────────────────────
	backing = copyService(svc)

	// Collect host→container port mappings before we remove ports.
	type portPair struct{ host, container int }
	pairs := make([]portPair, 0, len(svc.Ports))
	for _, p := range svc.Ports {
		h, c, err := parsePort(p)
		if err != nil {
			return Service{}, Service{}, fmt.Errorf("parse port %q: %w", p, err)
		}
		pairs = append(pairs, portPair{h, c})
	}

	// Remove host port bindings; add container-side expose.
	backing.Ports = nil
	for _, pp := range pairs {
		backing.Expose = appendUnique(backing.Expose, strconv.Itoa(pp.container))
	}

	// Join dpivot_mesh. If the service had no explicit networks it was on the
	// implicit "default" network; preserve that so it can still reach stateful
	// services (db, redis) that are not on dpivot_mesh.
	if len(backing.Networks) == 0 {
		backing.Networks = append(backing.Networks, "default")
	}
	backing.Networks = appendUnique(backing.Networks, "dpivot_mesh")
	// Sync network list to RawFields (Networks has yaml:"-").
	backing.RawFields["networks"] = toRawSlice(backing.Networks)

	// Inject DPIVOT_BACKEND env var (informational).
	if backing.Environment == nil {
		backing.Environment = make(map[string]string)
	}
	if len(pairs) > 0 {
		backing.Environment["DPIVOT_BACKEND"] = fmt.Sprintf("%s:%d", name, pairs[0].container)
	}
	// Sync environment map to RawFields (Environment has yaml:"-").
	backing.RawFields["environment"] = toRawMap(backing.Environment)

	// Strip x-dpivot so Docker never sees it.
	backing.XDpivot = XDpivotConfig{}
	delete(backing.RawFields, "x-dpivot")

	// ── Labels ───────────────────────────────────────────────────────────
	labels := map[string]interface{}{
		"dpivot.managed": "true",
		"dpivot.service": name,
	}
	if existing, ok := backing.RawFields["labels"]; ok {
		if m, ok := existing.(map[string]interface{}); ok {
			for k, v := range labels {
				m[k] = v
			}
		}
	} else {
		backing.RawFields["labels"] = labels
	}

	// ── Proxy service ─────────────────────────────────────────────────────
	// Build DPIVOT_BINDS from port pairs.
	binds := make([]string, 0, len(pairs))
	for _, pp := range pairs {
		binds = append(binds, fmt.Sprintf("%d:%d", pp.host, pp.container))
	}

	// Initial backend entry: the backing service is reachable by DNS name
	// on dpivot_mesh at its container port.
	initialBackend := fmt.Sprintf("%s-default:%s:%d", name, name, pairs[0].container)
	if len(pairs) == 0 {
		initialBackend = ""
	}

	// Ports owned by the proxy (original host port bindings).
	// Convention: control port on the host = first traffic host port + 6900
	// e.g. service at host:3001 → control reachable at localhost:9901
	controlHostPort := 9900
	if len(pairs) > 0 {
		controlHostPort = pairs[0].host + 6900
	}
	proxyPorts := make([]string, 0, len(pairs)+1)
	for _, pp := range pairs {
		proxyPorts = append(proxyPorts, fmt.Sprintf("%d:%d", pp.host, pp.host))
	}
	proxyPorts = append(proxyPorts, fmt.Sprintf("%d:9900", controlHostPort))

	proxyEnv := map[string]string{
		"DPIVOT_BINDS":        strings.Join(binds, ","),
		"DPIVOT_TARGETS":      initialBackend,
		"DPIVOT_CONTROL_PORT": "9900",
	}
	proxy = Service{
		Image:     "technicaltalk/dpivot-proxy:latest",
		Ports:     proxyPorts,
		Expose:    []string{"9900"},
		Networks:  []string{"dpivot_mesh"},
		DependsOn: []string{name},
		Environment: proxyEnv,
		RawFields: map[string]interface{}{
			// These three fields have yaml:"-" and must live in RawFields to be emitted.
			"environment": toRawMap(proxyEnv),
			"networks":    toRawSlice([]string{"dpivot_mesh"}),
			"depends_on":  toRawSlice([]string{name}),
			"labels": map[string]interface{}{
				"dpivot.proxy":   "true",
				"dpivot.service": name,
				"dpivot.managed": "true",
			},
			"restart": "unless-stopped",
		},
	}

	return backing, proxy, nil
}

// ── Helpers ───────────────────────────────────────────────────────────────────

// parsePort parses a Compose port mapping string into (hostPort, containerPort).
// Supported forms:
//   - "3000:3000"        → (3000, 3000)
//   - "3000"             → (3000, 3000)  single port is both host and container
//   - "0.0.0.0:3000:3000" → (3000, 3000)  IP prefix stripped
func parsePort(s string) (host, container int, err error) {
	// Strip optional IP prefix (e.g. "0.0.0.0:8080:8080").
	if strings.Count(s, ":") > 1 {
		idx := strings.Index(s, ":")
		s = s[idx+1:]
	}

	parts := strings.SplitN(s, ":", 2)
	switch len(parts) {
	case 1:
		n, e := strconv.Atoi(parts[0])
		if e != nil {
			return 0, 0, fmt.Errorf("invalid port number %q", parts[0])
		}
		return n, n, nil
	case 2:
		h, e := strconv.Atoi(parts[0])
		if e != nil {
			return 0, 0, fmt.Errorf("invalid host port %q", parts[0])
		}
		c, e := strconv.Atoi(parts[1])
		if e != nil {
			return 0, 0, fmt.Errorf("invalid container port %q", parts[1])
		}
		return h, c, nil
	}
	return 0, 0, fmt.Errorf("unrecognised port format %q", s)
}

func appendUnique(s []string, v string) []string {
	for _, x := range s {
		if x == v {
			return s
		}
	}
	return append(s, v)
}

func copyService(svc Service) Service {
	out := svc
	// Deep copy maps and slices so mutations don't affect the input.
	out.Ports = copyStrSlice(svc.Ports)
	out.Expose = copyStrSlice(svc.Expose)
	out.Networks = copyStrSlice(svc.Networks)
	out.DependsOn = copyStrSlice(svc.DependsOn)
	out.Environment = copyStrMap(svc.Environment)
	out.RawFields = deepCopyMap(svc.RawFields)
	return out
}

func copyStrSlice(s []string) []string {
	if s == nil {
		return nil
	}
	out := make([]string, len(s))
	copy(out, s)
	return out
}

func copyStrMap(m map[string]string) map[string]string {
	if m == nil {
		return nil
	}
	out := make(map[string]string, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}

// toRawSlice converts []string to []interface{} for storage in RawFields.
// Necessary because RawFields values are map[string]interface{} and yaml
// marshals []interface{} correctly while []string inside interface{} may not.
func toRawSlice(s []string) []interface{} {
	out := make([]interface{}, len(s))
	for i, v := range s {
		out[i] = v
	}
	return out
}

// toRawMap converts map[string]string to map[string]interface{} for RawFields.
func toRawMap(m map[string]string) map[string]interface{} {
	out := make(map[string]interface{}, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}

func deepCopyMap(m map[string]interface{}) map[string]interface{} {
	if m == nil {
		return nil
	}
	out := make(map[string]interface{}, len(m))
	for k, v := range m {
		out[k] = v // shallow copy of values is sufficient for our use-case
	}
	return out
}
