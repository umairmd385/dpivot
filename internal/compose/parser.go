package compose

import (
	"fmt"
	"os"

	"gopkg.in/yaml.v3"
)

// ParseFile reads and parses a docker-compose.yml at path.
//
// Guarantees:
//   - Never modifies the source file.
//   - Handles missing fields gracefully (returns descriptive errors).
//   - Preserves all unrecognised fields in Service.RawFields so they are
//     written verbatim into the generated output.
//   - No env-var interpolation is performed: ${VAR} literals are preserved as-is.
//     This is intentional — the generated file is run by `docker compose`, which
//     performs its own interpolation at runtime.
func ParseFile(path string) (*ComposeFile, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("parse: read file %q: %w", path, err)
	}
	return ParseBytes(data)
}

// ParseBytes parses a docker-compose.yml from raw bytes.
// The same guarantees as ParseFile apply.
func ParseBytes(data []byte) (*ComposeFile, error) {
	// First pass: decode into a raw yaml.Node to preserve key order and anchors.
	var raw yaml.Node
	if err := yaml.Unmarshal(data, &raw); err != nil {
		return nil, fmt.Errorf("parse: invalid YAML: %w", err)
	}
	if raw.Kind == 0 {
		return nil, fmt.Errorf("parse: file is empty")
	}

	// Second pass: decode into typed structs.
	var cf composeFileRaw
	if err := raw.Decode(&cf); err != nil {
		return nil, fmt.Errorf("parse: decode compose file: %w", err)
	}
	if len(cf.Services) == 0 {
		return nil, fmt.Errorf("parse: no services defined")
	}

	result := &ComposeFile{
		Version:  cf.Version,
		Services: make(map[string]Service, len(cf.Services)),
		Networks: cf.Networks,
		Volumes:  cf.Volumes,
	}

	for name, raw := range cf.Services {
		svc, err := coerceService(name, raw)
		if err != nil {
			return nil, fmt.Errorf("parse: service %q: %w", name, err)
		}
		result.Services[name] = svc
	}
	return result, nil
}

// ── Internal raw types ────────────────────────────────────────────────────────

// composeFileRaw mirrors ComposeFile but uses interface{} for services so we
// can deserialise them ourselves.
type composeFileRaw struct {
	Version  string                        `yaml:"version"`
	Services map[string]map[string]interface{} `yaml:"services"`
	Networks map[string]interface{}         `yaml:"networks"`
	Volumes  map[string]interface{}         `yaml:"volumes"`
}

// coerceService converts a raw map (one service stanza from the YAML) into a
// Service struct, carefully decoding the polymorphic fields (ports, networks,
// depends_on, environment) that can be expressed in multiple YAML shapes.
func coerceService(name string, raw map[string]interface{}) (Service, error) {
	svc := Service{
		RawFields: make(map[string]interface{}),
	}

	for k, v := range raw {
		switch k {
		case "image":
			svc.Image, _ = v.(string)

		case "ports":
			ports, err := coerceStringSlice(v)
			if err != nil {
				return Service{}, fmt.Errorf("ports: %w", err)
			}
			svc.Ports = ports

		case "expose":
			exp, err := coerceStringSlice(v)
			if err != nil {
				return Service{}, fmt.Errorf("expose: %w", err)
			}
			svc.Expose = exp

		case "environment":
			env, err := coerceEnvironment(v)
			if err != nil {
				return Service{}, fmt.Errorf("environment: %w", err)
			}
			svc.Environment = env

		case "networks":
			nets, err := coerceStringSlice(v)
			if err != nil {
				// networks can also be a map (named networks with options)
				svc.RawFields[k] = v
				continue
			}
			svc.Networks = nets

		case "depends_on":
			deps, err := coerceStringSlice(v)
			if err != nil {
				// depends_on can be map-form (with condition)
				svc.RawFields[k] = v
				continue
			}
			svc.DependsOn = deps

		case "x-dpivot":
			// Decode x-dpivot block manually.
			if m, ok := v.(map[string]interface{}); ok {
				if skip, ok := m["skip"].(bool); ok {
					svc.XDpivot.Skip = skip
				}
			}
			// Do NOT add x-dpivot to RawFields — it will be stripped.

		default:
			svc.RawFields[k] = v
		}
	}
	return svc, nil
}

// coerceStringSlice converts the polymorphic YAML list-or-scalar into []string.
// Docker Compose allows both:
//   - "ports: [\"3000:3000\"]"  (sequence)
//   - "ports:\n  - 3000:3000"  (block sequence)
func coerceStringSlice(v interface{}) ([]string, error) {
	switch val := v.(type) {
	case []interface{}:
		out := make([]string, 0, len(val))
		for _, item := range val {
			s, ok := item.(string)
			if !ok {
				// Docker Compose also supports short numeric port form
				s = fmt.Sprintf("%v", item)
			}
			out = append(out, s)
		}
		return out, nil
	case string:
		return []string{val}, nil
	case nil:
		return nil, nil
	default:
		return nil, fmt.Errorf("unexpected type %T", v)
	}
}

// coerceEnvironment handles both the map form and the list form of environment:
//
//	# Map form
//	environment:
//	  KEY: value
//
//	# List form
//	environment:
//	  - KEY=value
func coerceEnvironment(v interface{}) (map[string]string, error) {
	switch val := v.(type) {
	case map[string]interface{}:
		out := make(map[string]string, len(val))
		for k, vv := range val {
			out[k] = fmt.Sprintf("%v", vv)
		}
		return out, nil

	case []interface{}:
		out := make(map[string]string, len(val))
		for _, item := range val {
			s, ok := item.(string)
			if !ok {
				continue
			}
			if idx := indexOf(s, '='); idx >= 0 {
				out[s[:idx]] = s[idx+1:]
			} else {
				out[s] = "" // key-only entry (value from host env)
			}
		}
		return out, nil

	case nil:
		return nil, nil
	default:
		return nil, fmt.Errorf("unexpected environment type %T", v)
	}
}

func indexOf(s string, b byte) int {
	for i := 0; i < len(s); i++ {
		if s[i] == b {
			return i
		}
	}
	return -1
}
