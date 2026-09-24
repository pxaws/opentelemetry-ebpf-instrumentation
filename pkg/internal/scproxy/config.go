// Copyright The OpenTelemetry Authors
// SPDX-License-Identifier: Apache-2.0

package scproxy // import "go.opentelemetry.io/obi/pkg/internal/scproxy"

import "time"

const (
	DefaultInterval = 10 * time.Second
	DefaultProcRoot = "/proc"
)

// SCProxyConfig enables naming the callee of calls made through a Service Connect VIP.
// EXPERIMENTAL (PoC), off by default.
type SCProxyConfig struct {
	Enabled bool `yaml:"enabled" env:"OTEL_EBPF_SCPROXY_ENABLED"`
	// Interval between two reads of every sidecar's admin socket.
	Interval time.Duration `yaml:"interval" env:"OTEL_EBPF_SCPROXY_INTERVAL"`
	// BlockListFile holds network namespace inodes, one decimal per line, whose admin
	// reads are treated as failed. PoC-only fault injection; re-read on every tick.
	// A missing file is an empty list.
	BlockListFile string `yaml:"block_list_file" env:"OTEL_EBPF_SCPROXY_BLOCK_LIST_FILE"`
	ProcRoot      string `yaml:"proc_root" env:"OTEL_EBPF_SCPROXY_PROC_ROOT"`
}

func (c *SCProxyConfig) ProcRootOrDefault() string {
	if c.ProcRoot == "" {
		return DefaultProcRoot
	}
	return c.ProcRoot
}
