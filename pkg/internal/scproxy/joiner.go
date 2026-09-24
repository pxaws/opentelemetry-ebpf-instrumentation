// Copyright The OpenTelemetry Authors
// SPDX-License-Identifier: Apache-2.0

package scproxy // import "go.opentelemetry.io/obi/pkg/internal/scproxy"

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"io/fs"
	"log/slog"
	"net/netip"
	"os"
	"strconv"
	"sync"
	"sync/atomic"
	"time"
)

// readBudget bounds one tick's admin reads as a whole. Reads run in parallel, so one
// slow sidecar costs at most one DefaultAdminTimeout and cannot stall the others.
const readBudget = 2 * DefaultAdminTimeout

// netnsTables maps a task network namespace inode to that task's VIP -> callee name.
// A published value is never modified.
type netnsTables map[uint64]map[netip.AddrPort]string

// Joiner periodically joins each sidecar's VIP -> backends table with the names learned
// per backend address, and publishes the result for lock-free lookups from the span
// pipeline.
type Joiner struct {
	log           *slog.Logger
	interval      time.Duration
	blockListFile string
	findSidecars  func() ([]Sidecar, error)
	readVIPTable  func(ctx context.Context, sc Sidecar) (VIPTable, error)
	peers         PeerNameSource
	tables        atomic.Pointer[netnsTables]
}

func NewJoiner(cfg *SCProxyConfig, peers PeerNameSource) *Joiner {
	interval := cfg.Interval
	if interval <= 0 {
		interval = DefaultInterval
	}
	procRoot := cfg.ProcRootOrDefault()
	return &Joiner{
		log:           slog.With("component", "scproxy.Joiner"),
		interval:      interval,
		blockListFile: cfg.BlockListFile,
		findSidecars:  func() ([]Sidecar, error) { return FindSidecars(procRoot) },
		readVIPTable: func(ctx context.Context, sc Sidecar) (VIPTable, error) {
			return NewAdminClient(sc.AdminSocket, DefaultAdminTimeout).ReadVIPTable(ctx)
		},
		peers: peers,
	}
}

// Run refreshes the tables every interval until ctx is done.
func (j *Joiner) Run(ctx context.Context) {
	ticker := time.NewTicker(j.interval)
	defer ticker.Stop()
	for {
		j.tick(ctx)
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

// Lookup returns the callee name of vip as seen from network namespace netns.
func (j *Joiner) Lookup(netns uint64, vip netip.AddrPort) (string, bool) {
	tables := j.tables.Load()
	if tables == nil {
		return "", false
	}
	name, ok := (*tables)[netns][vip]
	return name, ok
}

func (j *Joiner) tick(ctx context.Context) {
	sidecars, err := j.findSidecars()
	if err != nil {
		j.log.Debug("can't list sidecars", "error", err)
	}

	vipTables := j.readVIPTables(ctx, sidecars, readBlockList(j.blockListFile))

	peers, err := j.peers.PeerNames()
	if err != nil {
		// Expected until tpinjector loads; the next tick retries.
		j.log.Debug("no peer names available", "error", err)
		peers = nil
	}

	tables := make(netnsTables, len(vipTables))
	for netns, vips := range vipTables {
		tables[netns] = joinNames(vips, peers)
	}
	j.tables.Store(&tables)
}

// readVIPTables reads every sidecar's admin socket in parallel. A netns whose read
// fails, or that is block-listed, is missing from the result, so it loses its table.
func (j *Joiner) readVIPTables(ctx context.Context, sidecars []Sidecar, blocked map[uint64]struct{}) map[uint64]VIPTable {
	ctx, cancel := context.WithTimeout(ctx, readBudget)
	defer cancel()

	var mu sync.Mutex
	var wg sync.WaitGroup
	result := make(map[uint64]VIPTable, len(sidecars))
	for _, sc := range sidecars {
		if _, isBlocked := blocked[sc.NetNS]; isBlocked {
			j.log.Debug("netns is block-listed, treating admin read as failed", "netns", sc.NetNS)
			continue
		}
		wg.Go(func() {
			table, err := j.readVIPTable(ctx, sc)
			if err != nil {
				j.log.Debug("can't read sidecar admin socket", "netns", sc.NetNS, "pid", sc.PID, "error", err)
				return
			}
			mu.Lock()
			result[sc.NetNS] = table
			mu.Unlock()
		})
	}
	wg.Wait()
	return result
}

func joinNames(vips VIPTable, peers map[string]string) map[netip.AddrPort]string {
	names := map[netip.AddrPort]string{}
	for vip, backends := range vips {
		if name, ok := agreedName(backends, peers); ok {
			names[vip] = name
		}
	}
	return names
}

// agreedName returns the one name that every known current backend reports. Unknown
// backends are skipped; any disagreement means no name, never a guess. Only current
// backends are consulted, so a stale name on a reused IP outside the set cannot leak in.
func agreedName(backends []netip.AddrPort, peers map[string]string) (string, bool) {
	agreed := ""
	for _, backend := range backends {
		name, known := peers[backend.String()]
		if !known {
			continue
		}
		if agreed != "" && name != agreed {
			return "", false
		}
		agreed = name
	}
	return agreed, agreed != ""
}

// readBlockList parses one decimal netns inode per line. A missing or unset file is an
// empty list; unparsable lines are skipped.
func readBlockList(path string) map[uint64]struct{} {
	blocked := map[uint64]struct{}{}
	if path == "" {
		return blocked
	}
	content, err := os.ReadFile(path)
	if err != nil {
		if !errors.Is(err, fs.ErrNotExist) {
			slog.Debug("can't read scproxy block list", "path", path, "error", err)
		}
		return blocked
	}

	scanner := bufio.NewScanner(bytes.NewReader(content))
	for scanner.Scan() {
		netns, err := strconv.ParseUint(string(bytes.TrimSpace(scanner.Bytes())), 10, 64)
		if err != nil {
			continue
		}
		blocked[netns] = struct{}{}
	}
	return blocked
}
