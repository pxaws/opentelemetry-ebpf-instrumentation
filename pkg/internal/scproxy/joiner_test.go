// Copyright The OpenTelemetry Authors
// SPDX-License-Identifier: Apache-2.0

package scproxy

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	netnsA = uint64(4026532553)
	netnsB = uint64(4026532700)
)

type fakePeers map[string]string

func (f fakePeers) PeerNames() (map[string]string, error) { return f, nil }

type failingPeers struct{}

func (failingPeers) PeerNames() (map[string]string, error) {
	return nil, errors.New("svc_name_by_peer map is not loaded")
}

func sidecar(netns uint64) Sidecar {
	return Sidecar{PID: int32(netns % 1000), NetNS: netns, AdminSocket: fmt.Sprintf("/fake/%d/admin.sock", netns)}
}

// testJoiner returns a joiner whose admin reads come from tables, keyed by netns.
// A netns missing from tables fails its admin read.
func testJoiner(t *testing.T, sidecars []Sidecar, tables map[uint64]VIPTable, peers PeerNameSource) *Joiner {
	t.Helper()
	return &Joiner{
		log:           slog.Default(),
		interval:      DefaultInterval,
		blockListFile: filepath.Join(t.TempDir(), "block-list"),
		findSidecars:  func() ([]Sidecar, error) { return sidecars, nil },
		readVIPTable: func(_ context.Context, sc Sidecar) (VIPTable, error) {
			if table, ok := tables[sc.NetNS]; ok {
				return table, nil
			}
			return nil, errors.New("admin socket unreachable")
		},
		peers: peers,
	}
}

func probe07(t *testing.T) VIPTable {
	t.Helper()
	table, err := ParseVIPTable(readFixture(t, "probe-07-config-dump.json"), readFixture(t, "probe-07-clusters.json"))
	require.NoError(t, err)
	return table
}

func lookup(j *Joiner, netns uint64, vip string) (string, bool) {
	return j.Lookup(netns, ap(vip))
}

func TestJoiner_Probe07OneKnownBackend(t *testing.T) {
	j := testJoiner(t, []Sidecar{sidecar(netnsA)},
		map[uint64]VIPTable{netnsA: probe07(t)},
		fakePeers{"172.31.45.21:80": "blog-service"})
	j.tick(t.Context())

	name, ok := lookup(j, netnsA, "127.255.0.1:8080")
	assert.True(t, ok)
	assert.Equal(t, "blog-service", name)

	_, ok = lookup(j, netnsA, "127.255.0.2:8080")
	assert.False(t, ok)
	_, ok = lookup(j, netnsA, "127.255.0.3:9090")
	assert.False(t, ok)
}

func TestJoiner_DisagreeingBackendsGiveNoEntry(t *testing.T) {
	j := testJoiner(t, []Sidecar{sidecar(netnsA)},
		map[uint64]VIPTable{netnsA: probe07(t)},
		fakePeers{"172.31.45.21:80": "blog-service", "172.31.32.69:80": "shop-service"})
	j.tick(t.Context())

	_, ok := lookup(j, netnsA, "127.255.0.1:8080")
	assert.False(t, ok)
}

func TestJoiner_AgreeingBackendsGiveTheName(t *testing.T) {
	j := testJoiner(t, []Sidecar{sidecar(netnsA)},
		map[uint64]VIPTable{netnsA: probe07(t)},
		fakePeers{"172.31.45.21:80": "blog-service", "172.31.32.69:80": "blog-service"})
	j.tick(t.Context())

	name, ok := lookup(j, netnsA, "127.255.0.1:8080")
	assert.True(t, ok)
	assert.Equal(t, "blog-service", name)
}

func TestJoiner_PeerOutsideEveryEndpointSetIsIgnored(t *testing.T) {
	// 172.31.99.9 once served this VIP; its IP is now reused by an unrelated task.
	j := testJoiner(t, []Sidecar{sidecar(netnsA)},
		map[uint64]VIPTable{netnsA: probe07(t)},
		fakePeers{"172.31.45.21:80": "blog-service", "172.31.99.9:80": "other-service"})
	j.tick(t.Context())

	name, ok := lookup(j, netnsA, "127.255.0.1:8080")
	assert.True(t, ok)
	assert.Equal(t, "blog-service", name)
}

func TestJoiner_FailedAdminReadDropsOnlyThatNetns(t *testing.T) {
	tables := map[uint64]VIPTable{netnsA: probe07(t), netnsB: probe07(t)}
	j := testJoiner(t, []Sidecar{sidecar(netnsA), sidecar(netnsB)}, tables,
		fakePeers{"172.31.45.21:80": "blog-service"})
	j.tick(t.Context())
	_, ok := lookup(j, netnsA, "127.255.0.1:8080")
	require.True(t, ok)

	delete(tables, netnsA)
	j.tick(t.Context())

	_, ok = lookup(j, netnsA, "127.255.0.1:8080")
	assert.False(t, ok)
	name, ok := lookup(j, netnsB, "127.255.0.1:8080")
	assert.True(t, ok)
	assert.Equal(t, "blog-service", name)
}

func TestJoiner_BlockListIsReadEachTick(t *testing.T) {
	j := testJoiner(t, []Sidecar{sidecar(netnsA), sidecar(netnsB)},
		map[uint64]VIPTable{netnsA: probe07(t), netnsB: probe07(t)},
		fakePeers{"172.31.45.21:80": "blog-service"})

	j.tick(t.Context())
	_, ok := lookup(j, netnsA, "127.255.0.1:8080")
	require.True(t, ok)

	require.NoError(t, os.WriteFile(j.blockListFile, []byte("4026532553\n"), 0o600))
	j.tick(t.Context())
	_, ok = lookup(j, netnsA, "127.255.0.1:8080")
	assert.False(t, ok, "blocked netns must lose its table")
	_, ok = lookup(j, netnsB, "127.255.0.1:8080")
	assert.True(t, ok, "other netns must keep its table")

	require.NoError(t, os.WriteFile(j.blockListFile, nil, 0o600))
	j.tick(t.Context())
	_, ok = lookup(j, netnsA, "127.255.0.1:8080")
	assert.True(t, ok, "unblocked netns must get its table back")
}

func TestJoiner_EmptyHolderGivesNoEntries(t *testing.T) {
	j := testJoiner(t, []Sidecar{sidecar(netnsA)},
		map[uint64]VIPTable{netnsA: probe07(t)},
		&PeerNameHolder{})
	j.tick(t.Context())

	for vip := range probe07(t) {
		_, ok := j.Lookup(netnsA, vip)
		assert.False(t, ok, vip.String())
	}
}

func TestJoiner_PeerSourceErrorGivesNoEntries(t *testing.T) {
	holder := &PeerNameHolder{}
	holder.Set(failingPeers{})
	j := testJoiner(t, []Sidecar{sidecar(netnsA)},
		map[uint64]VIPTable{netnsA: probe07(t)},
		holder)
	j.tick(t.Context())

	_, ok := lookup(j, netnsA, "127.255.0.1:8080")
	assert.False(t, ok)
}

func TestJoiner_SameVIPInTwoNetnsResolvesSeparately(t *testing.T) {
	tableA := VIPTable{ap("127.255.0.1:8080"): {ap("172.31.45.21:80")}}
	tableB := VIPTable{ap("127.255.0.1:8080"): {ap("172.31.38.144:80")}}
	j := testJoiner(t, []Sidecar{sidecar(netnsA), sidecar(netnsB)},
		map[uint64]VIPTable{netnsA: tableA, netnsB: tableB},
		fakePeers{"172.31.45.21:80": "blog-service", "172.31.38.144:80": "cart-service"})
	j.tick(t.Context())

	name, ok := lookup(j, netnsA, "127.255.0.1:8080")
	assert.True(t, ok)
	assert.Equal(t, "blog-service", name)

	name, ok = lookup(j, netnsB, "127.255.0.1:8080")
	assert.True(t, ok)
	assert.Equal(t, "cart-service", name)
}

func TestJoiner_LookupBeforeFirstTick(t *testing.T) {
	j := testJoiner(t, nil, nil, fakePeers{})
	_, ok := lookup(j, netnsA, "127.255.0.1:8080")
	assert.False(t, ok)
}

func TestReadBlockList(t *testing.T) {
	path := filepath.Join(t.TempDir(), "block-list")
	assert.Empty(t, readBlockList(path), "missing file is an empty list")

	require.NoError(t, os.WriteFile(path, []byte("4026532553\n\n  4026532700 \nnot-a-number\n"), 0o600))
	assert.Equal(t, map[uint64]struct{}{netnsA: {}, netnsB: {}}, readBlockList(path))

	assert.Empty(t, readBlockList(""), "no configured file is an empty list")
}
