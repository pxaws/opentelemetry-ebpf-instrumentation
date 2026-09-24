// Copyright The OpenTelemetry Authors
// SPDX-License-Identifier: Apache-2.0

package scproxy // import "go.opentelemetry.io/obi/pkg/internal/scproxy"

import (
	"cmp"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
)

const (
	envoyBinary = "envoy"

	// Service Connect forces the admin interface onto this path inside the
	// sidecar container (ENVOY_ADMIN_MODE=UDS).
	adminSocketRel = "tmp/envoy_admin.sock"

	initPID = "1"
)

// Sidecar is one Service Connect sidecar Envoy, one per task network namespace.
type Sidecar struct {
	PID   int32
	NetNS uint64 // inode of /proc/<pid>/ns/net; the key VIP state is kept under
	// AdminSocket is reached through /proc/<pid>/root, so the host process
	// needs no setns and no mount of its own.
	AdminSocket string
}

// FindSidecars scans procRoot (normally "/proc") for Envoy processes and
// returns one per network namespace, sorted by NetNS.
//
// Envoys in the host network namespace are skipped: that is the xDS relay,
// which runs from the same image and has its own admin socket, but carries no
// VIP listeners. Inside a task netns the pid is chosen by whether its mount
// namespace holds the admin socket, never by process-table order.
func FindSidecars(procRoot string) ([]Sidecar, error) {
	log := slog.With("component", "scproxy.FindSidecars")

	all, err := findTaskEnvoys(procRoot)
	if err != nil {
		return nil, err
	}

	byNetNS := map[uint64]Sidecar{}
	for _, sc := range all {
		// ReadDir sorts names lexically, not numerically, so keep the lowest
		// pid explicitly for a stable choice.
		if prev, seen := byNetNS[sc.NetNS]; seen && prev.PID < sc.PID {
			continue
		}
		byNetNS[sc.NetNS] = sc
	}

	sidecars := make([]Sidecar, 0, len(byNetNS))
	for _, sc := range byNetNS {
		sidecars = append(sidecars, sc)
	}
	slices.SortFunc(sidecars, func(a, b Sidecar) int { return cmp.Compare(a.NetNS, b.NetNS) })

	log.Debug("found sidecar envoys", "count", len(sidecars))
	return sidecars, nil
}

// FindEnvoyPIDs returns every sidecar Envoy pid, sorted, grouped by network
// namespace, with the same rules as FindSidecars. A sidecar may run more than
// one envoy process, and each one can write to a connection.
func FindEnvoyPIDs(procRoot string) (map[uint64][]int32, error) {
	all, err := findTaskEnvoys(procRoot)
	if err != nil {
		return nil, err
	}

	byNetNS := map[uint64][]int32{}
	for _, sc := range all {
		byNetNS[sc.NetNS] = append(byNetNS[sc.NetNS], sc.PID)
	}
	for _, pids := range byNetNS {
		slices.Sort(pids)
	}
	return byNetNS, nil
}

// ReadNetNS returns the network namespace inode of pid.
func ReadNetNS(procRoot string, pid int32) (uint64, error) {
	return readNSInode(procRoot, strconv.Itoa(int(pid)), "net")
}

// findTaskEnvoys lists every Envoy pid with a reachable admin socket outside
// the host network namespace.
func findTaskEnvoys(procRoot string) ([]Sidecar, error) {
	hostNetNS, err := readNSInode(procRoot, initPID, "net")
	if err != nil {
		return nil, fmt.Errorf("reading host network namespace: %w", err)
	}

	entries, err := os.ReadDir(procRoot)
	if err != nil {
		return nil, fmt.Errorf("listing %s: %w", procRoot, err)
	}

	var found []Sidecar
	for _, e := range entries {
		sc, ok := inspectPid(procRoot, e.Name())
		if !ok || sc.NetNS == hostNetNS {
			continue
		}
		found = append(found, sc)
	}
	return found, nil
}

// inspectPid reports whether pid is an Envoy with a reachable admin socket.
// Any read error means the process is gone or not ours to inspect.
func inspectPid(procRoot, name string) (Sidecar, bool) {
	pid, err := strconv.ParseInt(name, 10, 32)
	if err != nil {
		return Sidecar{}, false
	}
	if !isEnvoy(procRoot, name) {
		return Sidecar{}, false
	}

	netNS, err := readNSInode(procRoot, name, "net")
	if err != nil {
		return Sidecar{}, false
	}

	sock := filepath.Join(procRoot, name, "root", adminSocketRel)
	if info, err := os.Stat(sock); err != nil || info.IsDir() {
		return Sidecar{}, false
	}

	return Sidecar{PID: int32(pid), NetNS: netNS, AdminSocket: sock}, true
}

// isEnvoy prefers the exe link, which a process cannot rename. comm is the
// fallback for when readlink is denied; "envoy" fits its 15-byte limit.
func isEnvoy(procRoot, pid string) bool {
	if exe, err := os.Readlink(filepath.Join(procRoot, pid, "exe")); err == nil {
		exe = strings.TrimSuffix(exe, " (deleted)")
		return filepath.Base(exe) == envoyBinary
	}

	comm, err := os.ReadFile(filepath.Join(procRoot, pid, "comm"))
	if err != nil {
		return false
	}
	return strings.TrimSpace(string(comm)) == envoyBinary
}

// readNSInode reads the namespace inode from the link text ("net:[4026532553]")
// rather than stat'ing the target, so it works on any OS and on a fake /proc.
func readNSInode(procRoot, pid, kind string) (uint64, error) {
	link, err := os.Readlink(filepath.Join(procRoot, pid, "ns", kind))
	if err != nil {
		return 0, err
	}
	return parseNSLink(link, kind)
}

func parseNSLink(link, kind string) (uint64, error) {
	inner, ok := strings.CutPrefix(link, kind+":[")
	if !ok {
		return 0, fmt.Errorf("unexpected %s namespace link %q", kind, link)
	}
	inner, ok = strings.CutSuffix(inner, "]")
	if !ok {
		return 0, fmt.Errorf("unexpected %s namespace link %q", kind, link)
	}
	return strconv.ParseUint(inner, 10, 64)
}
