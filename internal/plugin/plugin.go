// Package plugin implements the Docker CLI plugin interface for dpivot.
//
// Docker CLI plugins are binaries placed in ~/.docker/cli-plugins/ and named
// docker-<name>. When Docker invokes the plugin it sets argv[0] to the binary
// name and prepends the subcommand arguments. The plugin must:
//
//  1. Respond to "docker-cli-plugin-metadata" with plugin metadata JSON.
//  2. Respond to all other subcommands as defined by the Cobra command tree.
//
// In dpivot, a single binary serves both as the standalone `dpivot` binary
// and as the `docker-dpivot` plugin. The mode is detected via os.Args[0].
package plugin

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
)

// Metadata is the JSON structure required by the Docker CLI plugin contract.
// See: https://github.com/docker/cli/blob/master/cli-plugins/plugin/plugin.go
type Metadata struct {
	SchemaVersion    string `json:"SchemaVersion"`
	Vendor           string `json:"Vendor"`
	Version          string `json:"Version"`
	ShortDescription string `json:"ShortDescription"`
	URL              string `json:"URL"`
}

// DefaultMetadata returns the dpivot plugin metadata.
func DefaultMetadata(version string) Metadata {
	return Metadata{
		SchemaVersion:    "0.1.0",
		Vendor:           "dpivot",
		Version:          version,
		ShortDescription: "Zero-downtime deployments for Docker Compose",
		URL:              "https://github.com/dpivot/dpivot",
	}
}

// IsDockerPluginMode returns true when the binary is invoked as a Docker CLI
// plugin (argv[0] == "docker-dpivot" or the Docker CLI plugin env is set).
func IsDockerPluginMode() bool {
	base := filepath.Base(os.Args[0])
	return base == "docker-dpivot" ||
		os.Getenv("DOCKER_CLI_PLUGIN_ORIGINAL_CLI_COMMAND") != ""
}

// HandleMetadataRequest checks whether the first argument is the special
// Docker CLI metadata probe ("docker-cli-plugin-metadata") and, if so,
// prints the metadata JSON and returns true. The caller should exit(0)
// after this returns true.
func HandleMetadataRequest(version string) bool {
	if len(os.Args) < 2 || os.Args[1] != "docker-cli-plugin-metadata" {
		return false
	}
	m := DefaultMetadata(version)
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	if err := enc.Encode(m); err != nil {
		fmt.Fprintf(os.Stderr, "dpivot plugin: encode metadata: %v\n", err)
		os.Exit(1)
	}
	return true
}

// StripPluginArgs strips the "dpivot" sub-command prefix that Docker adds
// before the real arguments when running in plugin mode.
//
// Docker invokes the plugin as:
//
//	docker-dpivot dpivot rollout web
//
// The Cobra root command is configured as "dpivot", so we need:
//
//	dpivot rollout web
//
// This function removes the extra "dpivot" token at argv[1] when present.
func StripPluginArgs() {
	if !IsDockerPluginMode() {
		return
	}
	if len(os.Args) >= 2 && os.Args[1] == "dpivot" {
		os.Args = append(os.Args[:1], os.Args[2:]...)
	}
}
