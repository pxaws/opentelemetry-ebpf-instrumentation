// Copyright The OpenTelemetry Authors
// SPDX-License-Identifier: Apache-2.0

//go:build linux

package tpinjector

import (
	"bytes"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.opentelemetry.io/obi/pkg/appolly/app"
	"go.opentelemetry.io/obi/pkg/obi"
)

const (
	testHostNetNS = 4026531840
	testTaskNetNS = 4026532553
	testTaskMntNS = 4026532624
)

// fakeNameMap stands in for proxy_svc_name_by_pid and decodes each value back to its name.
type fakeNameMap struct {
	entries map[uint32]string
}

func newFakeNameMap() *fakeNameMap { return &fakeNameMap{entries: map[uint32]string{}} }

func (m *fakeNameMap) Put(key, value any) error {
	val := value.(*[svcNameMaxLen + 1]byte)
	m.entries[*key.(*uint32)] = string(val[:val[svcNameMaxLen]])
	return nil
}

func (m *fakeNameMap) Delete(key any) error {
	delete(m.entries, *key.(*uint32))
	return nil
}

type fakeProcess struct {
	pid       int32
	exe       string
	netNS     uint64
	startTime uint64
	adminSock bool
}

// writeProcess lays out the parts of /proc/<pid> the borrow loop reads. It replaces
// an existing entry, which is how a test models a reused pid.
func writeProcess(t *testing.T, root string, p fakeProcess) {
	t.Helper()
	dir := filepath.Join(root, strconv.Itoa(int(p.pid)))
	require.NoError(t, os.RemoveAll(dir))
	require.NoError(t, os.MkdirAll(filepath.Join(dir, "ns"), 0o755))

	require.NoError(t, os.Symlink(p.exe, filepath.Join(dir, "exe")))
	require.NoError(t, os.Symlink(fmt.Sprintf("net:[%d]", p.netNS), filepath.Join(dir, "ns", "net")))
	require.NoError(t, os.Symlink(fmt.Sprintf("mnt:[%d]", testTaskMntNS), filepath.Join(dir, "ns", "mnt")))

	// field 22 is starttime; fields 4..21 and 23..52 are zero
	stat := fmt.Sprintf("%d (%s) S%s %d%s\n", p.pid, filepath.Base(p.exe),
		strings.Repeat(" 0", 18), p.startTime, strings.Repeat(" 0", 30))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "stat"), []byte(stat), 0o644))

	if p.adminSock {
		sock := filepath.Join(dir, "root", "tmp", "envoy_admin.sock")
		require.NoError(t, os.MkdirAll(filepath.Dir(sock), 0o755))
		require.NoError(t, os.WriteFile(sock, nil, 0o600))
	}
}

func removeProcess(t *testing.T, root string, pid int32) {
	t.Helper()
	require.NoError(t, os.RemoveAll(filepath.Join(root, strconv.Itoa(int(pid)))))
}

func envoyIn(pid int32, netNS uint64) fakeProcess {
	return fakeProcess{pid: pid, exe: "/usr/bin/envoy", netNS: netNS, startTime: 1000, adminSock: true}
}

func appIn(pid int32, netNS uint64) fakeProcess {
	return fakeProcess{pid: pid, exe: "/usr/bin/python3", netNS: netNS, startTime: 900}
}

type borrowFixture struct {
	root  string
	names *fakeNameMap
	logs  *bytes.Buffer
	b     *proxyBorrower
}

func newBorrowFixture(t *testing.T) *borrowFixture {
	t.Helper()
	root := t.TempDir()
	writeProcess(t, root, fakeProcess{pid: 1, exe: "/usr/lib/systemd/systemd", netNS: testHostNetNS})

	names := newFakeNameMap()
	logs := &bytes.Buffer{}
	log := slog.New(slog.NewTextHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	return &borrowFixture{root: root, names: names, logs: logs, b: newProxyBorrower(root, names, log)}
}

func (f *borrowFixture) allow(t *testing.T, p fakeProcess, name string) {
	t.Helper()
	writeProcess(t, f.root, p)
	f.b.allowApp(app.PID(p.pid), name)
}

func TestProxyBorrow_OneAppNamesItsEnvoy(t *testing.T) {
	f := newBorrowFixture(t)
	writeProcess(t, f.root, envoyIn(100, testTaskNetNS))
	f.allow(t, appIn(110, testTaskNetNS), "catalog-service")

	f.b.reconcile()

	assert.Equal(t, map[uint32]string{100: "catalog-service"}, f.names.entries)
}

func TestProxyBorrow_EveryEnvoyPidInTheTaskIsNamed(t *testing.T) {
	f := newBorrowFixture(t)
	writeProcess(t, f.root, envoyIn(100, testTaskNetNS))
	writeProcess(t, f.root, envoyIn(101, testTaskNetNS))
	f.allow(t, appIn(110, testTaskNetNS), "catalog-service")

	f.b.reconcile()

	assert.Equal(t, map[uint32]string{100: "catalog-service", 101: "catalog-service"}, f.names.entries)
}

func TestProxyBorrow_TwoNamesAreAmbiguousUntilOneLeaves(t *testing.T) {
	f := newBorrowFixture(t)
	writeProcess(t, f.root, envoyIn(100, testTaskNetNS))
	f.allow(t, appIn(110, testTaskNetNS), "catalog-service")
	f.allow(t, appIn(111, testTaskNetNS), "cart-service")

	f.b.reconcile()
	assert.Empty(t, f.names.entries)

	f.b.blockApp(111)
	f.b.reconcile()
	assert.Equal(t, map[uint32]string{100: "catalog-service"}, f.names.entries)
}

func TestProxyBorrow_SecondAppNameRemovesTheEntry(t *testing.T) {
	f := newBorrowFixture(t)
	writeProcess(t, f.root, envoyIn(100, testTaskNetNS))
	f.allow(t, appIn(110, testTaskNetNS), "catalog-service")
	f.b.reconcile()
	require.Len(t, f.names.entries, 1)

	f.allow(t, appIn(111, testTaskNetNS), "cart-service")
	f.b.reconcile()
	assert.Empty(t, f.names.entries)
}

func TestProxyBorrow_SameNameTwiceIsOneName(t *testing.T) {
	f := newBorrowFixture(t)
	writeProcess(t, f.root, envoyIn(100, testTaskNetNS))
	f.allow(t, appIn(110, testTaskNetNS), "catalog-service")
	f.allow(t, appIn(111, testTaskNetNS), "catalog-service")

	f.b.reconcile()

	assert.Equal(t, map[uint32]string{100: "catalog-service"}, f.names.entries)
}

func TestProxyBorrow_EnvoyWaitsForItsApp(t *testing.T) {
	f := newBorrowFixture(t)
	writeProcess(t, f.root, envoyIn(100, testTaskNetNS))

	f.b.reconcile()
	assert.Empty(t, f.names.entries)

	f.allow(t, appIn(110, testTaskNetNS), "catalog-service")
	f.b.reconcile()
	assert.Equal(t, map[uint32]string{100: "catalog-service"}, f.names.entries)
}

func TestProxyBorrow_RelayInHostNetNSIsSkipped(t *testing.T) {
	f := newBorrowFixture(t)
	writeProcess(t, f.root, envoyIn(400, testHostNetNS))
	f.allow(t, appIn(410, testHostNetNS), "node-agent")

	f.b.reconcile()

	assert.Empty(t, f.names.entries)
}

func TestProxyBorrow_OtherTasksDoNotLeakIn(t *testing.T) {
	f := newBorrowFixture(t)
	const otherNetNS = testTaskNetNS + 1
	writeProcess(t, f.root, envoyIn(100, testTaskNetNS))
	writeProcess(t, f.root, envoyIn(200, otherNetNS))
	f.allow(t, appIn(110, testTaskNetNS), "catalog-service")
	f.allow(t, appIn(210, otherNetNS), "cart-service")

	f.b.reconcile()

	assert.Equal(t, map[uint32]string{100: "catalog-service", 200: "cart-service"}, f.names.entries)
}

func TestProxyBorrow_LongNameIsDroppedNotTruncated(t *testing.T) {
	f := newBorrowFixture(t)
	long := strings.Repeat("x", svcNameMaxLen+1)
	writeProcess(t, f.root, envoyIn(100, testTaskNetNS))
	f.allow(t, appIn(110, testTaskNetNS), long)

	f.b.reconcile()

	assert.Empty(t, f.names.entries)
	assert.Contains(t, f.logs.String(), "exceeds TCP-option limit")
	assert.Contains(t, f.logs.String(), long)
}

func TestProxyBorrow_ReusedPidDoesNotKeepTheName(t *testing.T) {
	f := newBorrowFixture(t)
	writeProcess(t, f.root, envoyIn(100, testTaskNetNS))
	f.allow(t, appIn(110, testTaskNetNS), "catalog-service")
	f.b.reconcile()
	require.Equal(t, map[uint32]string{100: "catalog-service"}, f.names.entries)

	// pid 100 now belongs to an unrelated process in another task
	writeProcess(t, f.root, fakeProcess{pid: 100, exe: "/usr/bin/python3", netNS: testTaskNetNS + 1, startTime: 5000})
	f.b.reconcile()
	assert.Empty(t, f.names.entries)
}

func TestProxyBorrow_ReusedPidByAnotherEnvoyIsRewritten(t *testing.T) {
	f := newBorrowFixture(t)
	const otherNetNS = testTaskNetNS + 1
	writeProcess(t, f.root, envoyIn(100, testTaskNetNS))
	f.allow(t, appIn(110, testTaskNetNS), "catalog-service")
	f.allow(t, appIn(210, otherNetNS), "cart-service")
	f.b.reconcile()
	require.Equal(t, map[uint32]string{100: "catalog-service"}, f.names.entries)

	reused := envoyIn(100, otherNetNS)
	reused.startTime = 5000
	writeProcess(t, f.root, reused)
	f.b.reconcile()
	assert.Equal(t, map[uint32]string{100: "cart-service"}, f.names.entries)
}

func TestProxyBorrow_ExitedEnvoyIsDeleted(t *testing.T) {
	f := newBorrowFixture(t)
	writeProcess(t, f.root, envoyIn(100, testTaskNetNS))
	f.allow(t, appIn(110, testTaskNetNS), "catalog-service")
	f.b.reconcile()
	require.Len(t, f.names.entries, 1)

	removeProcess(t, f.root, 100)
	f.b.reconcile()
	assert.Empty(t, f.names.entries)
}

// An Envoy that discovery did not exclude has its own name; it must not count as the app.
func TestProxyBorrow_DiscoveredEnvoyIsNotAnApp(t *testing.T) {
	f := newBorrowFixture(t)
	writeProcess(t, f.root, envoyIn(100, testTaskNetNS))
	f.b.allowApp(100, "envoy")
	f.allow(t, appIn(110, testTaskNetNS), "catalog-service")

	f.b.reconcile()

	assert.Equal(t, map[uint32]string{100: "catalog-service"}, f.names.entries)
}

func TestProxyBorrow_OnlyWithTCPPropagation(t *testing.T) {
	for mode, want := range map[string]bool{"disabled": false, "headers": false, "tcp": true, "all": true} {
		cfg := &obi.Config{}
		cfg.SCProxy.Enabled = true
		require.NoError(t, cfg.EBPF.ContextPropagation.UnmarshalText([]byte(mode)))
		assert.Equal(t, want, New(cfg).proxyNames != nil, mode)
	}
}

// TCP propagation alone must not start the /proc scan or write the proxy map.
func TestProxyBorrow_OffUnlessSCProxyEnabled(t *testing.T) {
	for _, mode := range []string{"tcp", "all"} {
		cfg := &obi.Config{}
		require.NoError(t, cfg.EBPF.ContextPropagation.UnmarshalText([]byte(mode)))
		assert.Nil(t, New(cfg).proxyNames, mode)
	}
}

func TestNew_BorrowerUsesConfiguredProcRoot(t *testing.T) {
	cfg := &obi.Config{}
	cfg.SCProxy.Enabled = true
	require.NoError(t, cfg.EBPF.ContextPropagation.UnmarshalText([]byte("tcp")))
	cfg.SCProxy.ProcRoot = "/host/proc"
	require.NotNil(t, New(cfg).proxyNames)
	assert.Equal(t, "/host/proc", New(cfg).proxyNames.procRoot)

	cfg.SCProxy.ProcRoot = ""
	assert.Equal(t, "/proc", New(cfg).proxyNames.procRoot)
}
