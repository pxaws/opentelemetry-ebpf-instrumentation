// Copyright The OpenTelemetry Authors
// SPDX-License-Identifier: Apache-2.0

package scproxy

import (
	"os"
	"path/filepath"
	"strconv"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const hostNetNS = 4026531840

type fakeProc struct {
	pid    int32
	exe    string // empty: no exe link, as when readlink is denied
	comm   string
	netNS  uint64
	mntNS  uint64
	socket bool
}

func writeFakeProc(t *testing.T, root string, procs ...fakeProc) {
	t.Helper()
	for _, p := range procs {
		dir := filepath.Join(root, strconv.Itoa(int(p.pid)))
		require.NoError(t, os.MkdirAll(filepath.Join(dir, "ns"), 0o755))

		if p.exe != "" {
			require.NoError(t, os.Symlink(p.exe, filepath.Join(dir, "exe")))
		}
		if p.comm != "" {
			require.NoError(t, os.WriteFile(filepath.Join(dir, "comm"), []byte(p.comm+"\n"), 0o644))
		}
		// Link targets mirror the kernel's "net:[inode]" form; they dangle, which readlink does not mind.
		require.NoError(t, os.Symlink("net:["+strconv.FormatUint(p.netNS, 10)+"]", filepath.Join(dir, "ns", "net")))
		require.NoError(t, os.Symlink("mnt:["+strconv.FormatUint(p.mntNS, 10)+"]", filepath.Join(dir, "ns", "mnt")))

		if p.socket {
			sock := filepath.Join(dir, "root", adminSocketRel)
			require.NoError(t, os.MkdirAll(filepath.Dir(sock), 0o755))
			require.NoError(t, os.WriteFile(sock, nil, 0o600))
		}
	}
}

func hostInit() fakeProc {
	return fakeProc{pid: 1, exe: "/usr/lib/systemd/systemd", comm: "systemd", netNS: hostNetNS, mntNS: 4026531841}
}

func TestFindSidecars_SkipsRelayAndCollapsesPerNetNS(t *testing.T) {
	root := t.TempDir()
	writeFakeProc(t, root,
		hostInit(),
		fakeProc{pid: 400, exe: "/usr/bin/envoy", comm: "envoy", netNS: hostNetNS, mntNS: 4026532397, socket: true}, // xDS relay
		fakeProc{pid: 100, exe: "/usr/bin/envoy", comm: "envoy", netNS: 4026532553, mntNS: 4026532624, socket: true},
		fakeProc{pid: 101, exe: "/usr/bin/envoy", comm: "envoy", netNS: 4026532553, mntNS: 4026532624, socket: true},
		fakeProc{pid: 200, exe: "/usr/bin/envoy", comm: "envoy", netNS: 4026532600, mntNS: 4026532601, socket: true},
		fakeProc{pid: 300, exe: "/usr/bin/envoy", comm: "envoy", netNS: 4026532700, mntNS: 4026532701, socket: true},
		fakeProc{pid: 500, exe: "/usr/bin/python3", comm: "python3", netNS: 4026532800, mntNS: 4026532801, socket: true},
	)

	sidecars, err := FindSidecars(root)
	require.NoError(t, err)
	assert.Equal(t, []Sidecar{
		{PID: 100, NetNS: 4026532553, AdminSocket: filepath.Join(root, "100", "root", adminSocketRel)},
		{PID: 200, NetNS: 4026532600, AdminSocket: filepath.Join(root, "200", "root", adminSocketRel)},
		{PID: 300, NetNS: 4026532700, AdminSocket: filepath.Join(root, "300", "root", adminSocketRel)},
	}, sidecars)
}

// Inside one netns only a pid whose mount namespace holds the admin socket is usable.
func TestFindSidecars_PicksPidWithSocket(t *testing.T) {
	root := t.TempDir()
	writeFakeProc(t, root,
		hostInit(),
		fakeProc{pid: 100, exe: "/usr/bin/envoy", netNS: 4026532553, mntNS: 4026532900},
		fakeProc{pid: 150, exe: "/usr/bin/envoy", netNS: 4026532553, mntNS: 4026532624, socket: true},
		fakeProc{pid: 200, exe: "/usr/bin/envoy", netNS: 4026532600, mntNS: 4026532601},
	)

	sidecars, err := FindSidecars(root)
	require.NoError(t, err)
	require.Len(t, sidecars, 1)
	assert.Equal(t, int32(150), sidecars[0].PID)
}

func TestFindSidecars_FallsBackToComm(t *testing.T) {
	root := t.TempDir()
	writeFakeProc(t, root,
		hostInit(),
		fakeProc{pid: 100, comm: "envoy", netNS: 4026532553, mntNS: 4026532624, socket: true},
		fakeProc{pid: 200, comm: "bash", netNS: 4026532600, mntNS: 4026532601, socket: true},
	)

	sidecars, err := FindSidecars(root)
	require.NoError(t, err)
	require.Len(t, sidecars, 1)
	assert.Equal(t, int32(100), sidecars[0].PID)
}

func TestFindSidecars_DeletedExe(t *testing.T) {
	root := t.TempDir()
	writeFakeProc(t, root,
		hostInit(),
		fakeProc{pid: 100, exe: "/usr/bin/envoy (deleted)", netNS: 4026532553, mntNS: 4026532624, socket: true},
	)

	sidecars, err := FindSidecars(root)
	require.NoError(t, err)
	require.Len(t, sidecars, 1)
}

// Without the host netns the relay cannot be told apart, so refuse rather than guess.
func TestFindSidecars_NoHostNetNS(t *testing.T) {
	root := t.TempDir()
	writeFakeProc(t, root,
		fakeProc{pid: 100, exe: "/usr/bin/envoy", netNS: 4026532553, mntNS: 4026532624, socket: true},
	)

	sidecars, err := FindSidecars(root)
	require.Error(t, err)
	assert.Empty(t, sidecars)
}

func TestFindSidecars_IgnoresNonPidEntries(t *testing.T) {
	root := t.TempDir()
	writeFakeProc(t, root, hostInit())
	require.NoError(t, os.MkdirAll(filepath.Join(root, "sys"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(root, "uptime"), []byte("1 1\n"), 0o644))

	sidecars, err := FindSidecars(root)
	require.NoError(t, err)
	assert.Empty(t, sidecars)
}

func TestParseNSLink(t *testing.T) {
	inode, err := parseNSLink("net:[4026532553]", "net")
	require.NoError(t, err)
	assert.Equal(t, uint64(4026532553), inode)

	_, err = parseNSLink("mnt:[4026532553]", "net")
	require.Error(t, err)

	_, err = parseNSLink("net:[abc]", "net")
	require.Error(t, err)
}

func TestFindEnvoyPIDs_KeepsEveryPidPerNetNS(t *testing.T) {
	root := t.TempDir()
	writeFakeProc(t, root,
		hostInit(),
		fakeProc{pid: 400, exe: "/usr/bin/envoy", netNS: hostNetNS, mntNS: 4026532397, socket: true}, // xDS relay
		fakeProc{pid: 101, exe: "/usr/bin/envoy", netNS: 4026532553, mntNS: 4026532624, socket: true},
		fakeProc{pid: 100, exe: "/usr/bin/envoy", netNS: 4026532553, mntNS: 4026532624, socket: true},
		fakeProc{pid: 200, exe: "/usr/bin/envoy", netNS: 4026532600, mntNS: 4026532601, socket: true},
		fakeProc{pid: 500, exe: "/usr/bin/python3", netNS: 4026532600, mntNS: 4026532801, socket: true},
	)

	pids, err := FindEnvoyPIDs(root)
	require.NoError(t, err)
	assert.Equal(t, map[uint64][]int32{
		4026532553: {100, 101},
		4026532600: {200},
	}, pids)
}

func TestReadNetNS(t *testing.T) {
	root := t.TempDir()
	writeFakeProc(t, root, fakeProc{pid: 42, exe: "/usr/bin/python3", netNS: 4026532553, mntNS: 4026532624})

	inode, err := ReadNetNS(root, 42)
	require.NoError(t, err)
	assert.Equal(t, uint64(4026532553), inode)

	_, err = ReadNetNS(root, 43)
	require.Error(t, err)
}
