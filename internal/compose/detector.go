// Package compose provides parsing, detection, and generation of Docker
// Compose files for dpivot proxy injection.
package compose

import "strings"

// databaseImages lists image name substrings whose matching services are
// excluded from proxy injection by default. Matching is case-insensitive and
// tag/registry agnostic — only the base image name is checked.
var databaseImages = []string{
	"postgres",
	"postgresql",
	"mysql",
	"mariadb",
	"redis",
	"mongo",
	"mongodb",
	"elasticsearch",
	"opensearch",
	"cassandra",
	"couchdb",
	"influxdb",
	"rabbitmq",
	"kafka",
	"zookeeper",
	"mssql",
	"clickhouse",
	"minio",
	"vault",
}

// IsDatabase reports whether the given image name belongs to a known stateful
// database/cache/broker that must not be proxy-injected by default.
//
// Matching strips registry prefixes (e.g. "docker.io/library/") and image
// tags (e.g. ":latest", ":8.0") before comparing against the database list.
// Comparison is case-insensitive.
func IsDatabase(image string) bool {
	base := baseImageName(image)
	for _, db := range databaseImages {
		if strings.EqualFold(base, db) {
			return true
		}
	}
	return false
}

// ShouldProxy returns true when a service should receive proxy injection.
//
// Priority order (first matching rule wins):
//
//  1. x-dpivot.skip == true → never proxy (explicit opt-out)
//  2. No ports declared → never proxy (nothing to forward)
//  3. IsDatabase(image) == true → never proxy (safety: stateful service)
//  4. All other services with ports → proxy (auto-detection)
func ShouldProxy(svc Service) bool {
	if svc.XDpivot.Skip {
		return false
	}
	if len(svc.Ports) == 0 {
		return false
	}
	if IsDatabase(svc.Image) {
		return false
	}
	return true
}

// baseImageName strips the registry prefix and tag from an image reference,
// returning just the repository/image name component.
//
// Examples:
//
//	"postgres:16"                         → "postgres"
//	"docker.io/library/mysql:8.0"         → "mysql"
//	"gcr.io/my-project/myapp:abc123"      → "myapp"
//	"myorg/myapp"                         → "myapp"
func baseImageName(image string) string {
	// Strip tag.
	if idx := strings.LastIndex(image, ":"); idx >= 0 {
		// Make sure the colon is not part of a port in the hostname.
		rest := image[:idx]
		if !strings.Contains(image[idx:], "/") {
			image = rest
		}
	}
	// Strip registry + org prefix — keep only the rightmost path segment.
	if idx := strings.LastIndex(image, "/"); idx >= 0 {
		image = image[idx+1:]
	}
	return image
}

// DatabaseImages returns a copy of the known database image names list.
// Useful for documentation and test assertions.
func DatabaseImages() []string {
	out := make([]string, len(databaseImages))
	copy(out, databaseImages)
	return out
}
