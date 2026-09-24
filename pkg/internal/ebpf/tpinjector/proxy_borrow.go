// Copyright The OpenTelemetry Authors
// SPDX-License-Identifier: Apache-2.0

//go:build linux

package tpinjector // import "go.opentelemetry.io/obi/pkg/internal/ebpf/tpinjector"

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"time"

	"github.com/cilium/ebpf"
	"github.com/prometheus/procfs"

	"go.opentelemetry.io/obi/pkg/appolly/app"
	"go.opentelemetry.io/obi/pkg/internal/scproxy"
)

// EXPERIMENTAL — TCP service-name propagation.
//
// A Service Connect Envoy writes its task's responses but has no service name of its
// own. The borrower gives each sidecar Envoy pid the name of the one app OBI discovered
// in the same network namespace, through proxy_svc_name_by_pid. The BPF side stamps a
// borrowed name only toward non-loopback peers (bpf/tpinjector/proxy_stamp_policy.h).

// proxyReconcileInterval bounds how long a new sidecar goes unnamed and how long a
// stale entry can outlive its process.
const proxyReconcileInterval = 5 * time.Second

// proxyNameMap is the subset of *ebpf.Map the borrower writes, so tests can fake it.
type proxyNameMap interface {
	Put(key, value any) error
	Delete(key any) error
}

var errProxyMapNotLoaded = errors.New("proxy_svc_name_by_pid map is not loaded")

// lazyMap resolves the BPF map on each call: the tracer is built before its
// objects are loaded, and discovery can report apps before that.
type lazyMap struct {
	get func() *ebpf.Map
}

func (l lazyMap) Put(key, value any) error {
	m := l.get()
	if m == nil {
		return errProxyMapNotLoaded
	}
	return m.Put(key, value)
}

func (l lazyMap) Delete(key any) error {
	m := l.get()
	if m == nil {
		return errProxyMapNotLoaded
	}
	return m.Delete(key)
}

// proxyEntry is what the borrower wrote for one Envoy pid. The start time tells a
// reused pid apart from the process the name was written for.
type proxyEntry struct {
	startTime uint64
	name      string
	value     [svcNameMaxLen + 1]byte // name encoded as svc_name_value_t
}

type proxyBorrower struct {
	procRoot string
	names    proxyNameMap
	log      *slog.Logger

	// AllowPID/BlockPID (discovery goroutine) and reconcile (Run goroutine) both use
	// these, so every access holds mu.
	mu       sync.Mutex
	appNames map[app.PID]string
	appNetNS map[app.PID]uint64
	written  map[uint32]proxyEntry
}

func newProxyBorrower(procRoot string, names proxyNameMap, log *slog.Logger) *proxyBorrower {
	return &proxyBorrower{
		procRoot: procRoot,
		names:    names,
		log:      log,
		appNames: map[app.PID]string{},
		appNetNS: map[app.PID]uint64{},
		written:  map[uint32]proxyEntry{},
	}
}

// allowApp records a discovered app under its network namespace. An empty name is
// kept out: it is not a name the Envoy could borrow.
func (b *proxyBorrower) allowApp(pid app.PID, name string) {
	if name == "" {
		return
	}
	netNS, err := scproxy.ReadNetNS(b.procRoot, int32(pid))
	if err != nil {
		b.log.Debug("netns read failed, app not offered to its sidecar", "pid", pid, "error", err)
		return
	}

	b.mu.Lock()
	defer b.mu.Unlock()
	b.appNames[pid] = name
	b.appNetNS[pid] = netNS
}

func (b *proxyBorrower) blockApp(pid app.PID) {
	b.mu.Lock()
	defer b.mu.Unlock()
	delete(b.appNames, pid)
	delete(b.appNetNS, pid)
}

func (b *proxyBorrower) run(ctx context.Context) {
	ticker := time.NewTicker(proxyReconcileInterval)
	defer ticker.Stop()

	for {
		b.reconcile()
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

// reconcile brings proxy_svc_name_by_pid in line with the sidecars now running.
func (b *proxyBorrower) reconcile() {
	envoys, err := scproxy.FindEnvoyPIDs(b.procRoot)
	if err != nil {
		b.log.Debug("cannot list sidecar envoys", "error", err)
		return
	}

	b.mu.Lock()
	defer b.mu.Unlock()

	want := b.desiredEntries(envoys)

	for pid, have := range b.written {
		if _, keep := want[pid]; keep {
			continue
		}
		key := pid
		if err := b.names.Delete(&key); err != nil {
			b.log.Debug("failed to delete borrowed name", "pid", pid, "error", err)
		}
		delete(b.written, pid)
		b.log.Debug("sidecar no longer borrows a name", "pid", pid, "name", have.name)
	}

	for pid, entry := range want {
		if b.written[pid] == entry {
			continue
		}
		key := pid
		if err := b.names.Put(&key, &entry.value); err != nil {
			b.log.Debug("failed to write borrowed name", "pid", pid, "error", err)
			continue
		}
		b.written[pid] = entry
		b.log.Debug("sidecar borrows its app's name", "pid", pid, "name", entry.name)
	}
}

// desiredEntries names every Envoy pid in a netns that holds exactly one distinct app
// name. Zero names or several leave the sidecar unnamed: guessing would mislabel an edge.
func (b *proxyBorrower) desiredEntries(envoys map[uint64][]int32) map[uint32]proxyEntry {
	isEnvoy := map[app.PID]bool{}
	for _, pids := range envoys {
		for _, pid := range pids {
			isEnvoy[app.PID(pid)] = true
		}
	}

	namesByNetNS := map[uint64]map[string]struct{}{}
	for pid, name := range b.appNames {
		// an Envoy that discovery let through is not the task's app
		if isEnvoy[pid] {
			continue
		}
		netNS := b.appNetNS[pid]
		if namesByNetNS[netNS] == nil {
			namesByNetNS[netNS] = map[string]struct{}{}
		}
		namesByNetNS[netNS][name] = struct{}{}
	}

	// One procfs handle per tick: procRoot never changes, and NewFS stats the mount.
	fs, err := procfs.NewFS(b.procRoot)
	if err != nil {
		b.log.Debug("cannot open procfs", "root", b.procRoot, "error", err)
		return map[uint32]proxyEntry{}
	}

	want := map[uint32]proxyEntry{}
	for netNS, pids := range envoys {
		name, ok := soleName(namesByNetNS[netNS])
		if !ok {
			continue
		}
		value, ok := encodeServiceName(name)
		if !ok {
			b.log.Debug("service name exceeds TCP-option limit, sidecar not named",
				"name", name, "len", len(name), "limit", svcNameMaxLen, "netns", netNS)
			continue
		}
		for _, pid := range pids {
			startTime, err := startTime(fs, pid)
			if err != nil {
				continue // gone since the scan
			}
			want[uint32(pid)] = proxyEntry{startTime: startTime, name: name, value: value}
		}
	}
	return want
}

func soleName(names map[string]struct{}) (string, bool) {
	if len(names) != 1 {
		return "", false
	}
	for name := range names {
		return name, true
	}
	return "", false
}

// startTime reads field 22 of /proc/<pid>/stat.
func startTime(fs procfs.FS, pid int32) (uint64, error) {
	proc, err := fs.Proc(int(pid))
	if err != nil {
		return 0, err
	}
	stat, err := proc.Stat()
	if err != nil {
		return 0, err
	}
	return stat.Starttime, nil
}
