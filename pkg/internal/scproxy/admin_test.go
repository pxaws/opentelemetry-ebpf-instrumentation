// Copyright The OpenTelemetry Authors
// SPDX-License-Identifier: Apache-2.0

package scproxy

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/netip"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func readFixture(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", name))
	require.NoError(t, err)
	return b
}

func ap(s string) netip.AddrPort {
	return netip.MustParseAddrPort(s)
}

// probe07Table is the table measured on the sc-probe rig on 2026-09-23.
var probe07Table = VIPTable{
	ap("127.255.0.1:8080"): {ap("172.31.32.69:80"), ap("172.31.35.96:80"), ap("172.31.45.21:80")},
	ap("127.255.0.2:8080"): {ap("172.31.38.144:80")},
	ap("127.255.0.3:9090"): {ap("172.31.38.208:80")},
}

func TestParseVIPTable_Probe07(t *testing.T) {
	table, err := ParseVIPTable(readFixture(t, "probe-07-config-dump.json"), readFixture(t, "probe-07-clusters.json"))
	require.NoError(t, err)
	assert.Equal(t, probe07Table, table)
}

func TestParseVIPTable_SamePortDifferentVIPs(t *testing.T) {
	table, err := ParseVIPTable(readFixture(t, "probe-07-config-dump.json"), readFixture(t, "probe-07-clusters.json"))
	require.NoError(t, err)

	blog := table[ap("127.255.0.1:8080")]
	pay := table[ap("127.255.0.2:8080")]
	require.NotEmpty(t, blog)
	require.NotEmpty(t, pay)
	for _, b := range blog {
		assert.NotContains(t, pay, b)
	}
}

const tcpProxyDump = `{"configs":[
 {"@type":"type.googleapis.com/envoy.admin.v3.ListenersConfigDump","dynamic_listeners":[
  {"name":"egress","active_state":{"listener":{"name":"egress","traffic_direction":"OUTBOUND","filter_chains":[
   {"filter_chain_match":{"prefix_ranges":[{"address_prefix":"127.255.0.9","prefix_len":32}],"destination_port":5432},
    "filters":[{"name":"envoy.filters.network.tcp_proxy","typed_config":{
     "@type":"type.googleapis.com/envoy.extensions.filters.network.tcp_proxy.v3.TcpProxy","cluster":"db"}}]}
  ]}}}]}
]}`

func TestParseVIPTable_TCPProxyCluster(t *testing.T) {
	clusters := `{"cluster_statuses":[{"name":"db","host_statuses":[
	  {"address":{"socket_address":{"address":"10.0.0.5","port_value":5432}}}]}]}`

	table, err := ParseVIPTable([]byte(tcpProxyDump), []byte(clusters))
	require.NoError(t, err)
	assert.Equal(t, VIPTable{ap("127.255.0.9:5432"): {ap("10.0.0.5:5432")}}, table)
}

func TestParseVIPTable_ClusterWithoutFilterChainIsIgnored(t *testing.T) {
	clusters := `{"cluster_statuses":[
	  {"name":"db","host_statuses":[{"address":{"socket_address":{"address":"10.0.0.5","port_value":5432}}}]},
	  {"name":"orphan","host_statuses":[{"address":{"socket_address":{"address":"10.0.0.6","port_value":80}}}]}]}`

	table, err := ParseVIPTable([]byte(tcpProxyDump), []byte(clusters))
	require.NoError(t, err)
	assert.Len(t, table, 1)
	for _, backends := range table {
		assert.NotContains(t, backends, ap("10.0.0.6:80"))
	}
}

func TestParseVIPTable_TruncatedJSON(t *testing.T) {
	dump := readFixture(t, "probe-07-config-dump.json")
	clusters := readFixture(t, "probe-07-clusters.json")

	table, err := ParseVIPTable(dump[:len(dump)/2], clusters)
	require.Error(t, err)
	assert.Empty(t, table)

	table, err = ParseVIPTable(dump, clusters[:len(clusters)/2])
	require.Error(t, err)
	assert.Empty(t, table)
}

func TestParseVIPTable_NoBackendsYieldsEmptySets(t *testing.T) {
	table, err := ParseVIPTable(readFixture(t, "probe-07-config-dump.json"), []byte(`{}`))
	require.NoError(t, err)

	require.Len(t, table, len(probe07Table))
	for vip := range probe07Table {
		backends, ok := table[vip]
		assert.True(t, ok, vip)
		assert.Empty(t, backends, vip)
	}
}

// The EDS section is not consulted, so a dump fetched without include_eds
// still produces the full table.
func TestParseVIPTable_DumpWithoutEDS(t *testing.T) {
	dump := stripEDS(t, readFixture(t, "probe-07-config-dump.json"))

	table, err := ParseVIPTable(dump, readFixture(t, "probe-07-clusters.json"))
	require.NoError(t, err)
	assert.Equal(t, probe07Table, table)
}

// The xDS relay serves an ingress listener and no VIP chains.
func TestParseVIPTable_NoEgressListener(t *testing.T) {
	dump := `{"configs":[{"@type":"type.googleapis.com/envoy.admin.v3.ListenersConfigDump","dynamic_listeners":[
	  {"name":"ingress_listener_xds_relay","active_state":{"listener":{"name":"ingress_listener_xds_relay",
	   "filter_chains":[{"filters":[{"name":"envoy.filters.network.http_connection_manager"}]}]}}}]}]}`

	table, err := ParseVIPTable([]byte(dump), []byte(`{}`))
	require.NoError(t, err)
	assert.Empty(t, table)
}

func TestParseVIPTable_SkipsNonHostPrefix(t *testing.T) {
	dump := `{"configs":[{"@type":"type.googleapis.com/envoy.admin.v3.ListenersConfigDump","dynamic_listeners":[
	  {"name":"egress","active_state":{"listener":{"traffic_direction":"OUTBOUND","filter_chains":[
	   {"filter_chain_match":{"prefix_ranges":[{"address_prefix":"127.255.0.0","prefix_len":16}],"destination_port":80},
	    "filters":[{"name":"envoy.filters.network.tcp_proxy","typed_config":{"cluster":"db"}}]},
	   {"filter_chain_match":{"prefix_ranges":[{"address_prefix":"127.255.0.4","prefix_len":32}]},
	    "filters":[{"name":"envoy.filters.network.tcp_proxy","typed_config":{"cluster":"db"}}]}
	  ]}}}]}]}`

	table, err := ParseVIPTable([]byte(dump), []byte(`{}`))
	require.NoError(t, err)
	assert.Empty(t, table)
}

func stripEDS(t *testing.T, dump []byte) []byte {
	t.Helper()
	var doc map[string][]map[string]any
	require.NoError(t, json.Unmarshal(dump, &doc))

	var kept []map[string]any
	for _, c := range doc["configs"] {
		if c["@type"] == "type.googleapis.com/envoy.admin.v3.EndpointsConfigDump" {
			continue
		}
		kept = append(kept, c)
	}
	require.Len(t, kept, len(doc["configs"])-1)

	out, err := json.Marshal(map[string]any{"configs": kept})
	require.NoError(t, err)
	return out
}

// fakeAdmin serves the probe-07 fixtures on a unix socket and records every
// request method it sees.
type fakeAdmin struct {
	mu      sync.Mutex
	methods []string
	paths   []string
}

func (f *fakeAdmin) serve(t *testing.T, handler http.HandlerFunc) string {
	t.Helper()
	// t.TempDir can exceed the 108-byte sun_path limit.
	dir, err := os.MkdirTemp("", "scproxy")
	require.NoError(t, err)
	t.Cleanup(func() { os.RemoveAll(dir) })

	sock := filepath.Join(dir, "envoy_admin.sock")
	ln, err := net.Listen("unix", sock)
	require.NoError(t, err)

	srv := &http.Server{
		ReadHeaderTimeout: time.Second,
		Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			f.mu.Lock()
			f.methods = append(f.methods, r.Method)
			f.paths = append(f.paths, r.URL.Path)
			f.mu.Unlock()
			handler(w, r)
		}),
	}
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() { srv.Close() })
	return sock
}

func fixtureHandler(t *testing.T) http.HandlerFunc {
	dump := readFixture(t, "probe-07-config-dump.json")
	clusters := readFixture(t, "probe-07-clusters.json")
	return func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/config_dump":
			_, _ = w.Write(dump)
		case "/clusters":
			_, _ = w.Write(clusters)
		default:
			http.NotFound(w, r)
		}
	}
}

func TestAdminClient_ReadVIPTable_OnlyGET(t *testing.T) {
	fake := &fakeAdmin{}
	sock := fake.serve(t, fixtureHandler(t))

	table, err := NewAdminClient(sock, time.Second).ReadVIPTable(context.Background())
	require.NoError(t, err)
	assert.Equal(t, probe07Table, table)

	fake.mu.Lock()
	defer fake.mu.Unlock()
	require.NotEmpty(t, fake.methods)
	for _, m := range fake.methods {
		assert.Equal(t, http.MethodGet, m)
	}
	assert.ElementsMatch(t, []string{"/config_dump", "/clusters"}, fake.paths)
}

func TestAdminClient_SocketMissing(t *testing.T) {
	sock := filepath.Join(t.TempDir(), "missing.sock")

	table, err := NewAdminClient(sock, time.Second).ReadVIPTable(context.Background())
	require.Error(t, err)
	assert.Empty(t, table)
}

func TestAdminClient_Timeout(t *testing.T) {
	release := make(chan struct{})
	t.Cleanup(func() { close(release) })

	fake := &fakeAdmin{}
	sock := fake.serve(t, func(_ http.ResponseWriter, _ *http.Request) { <-release })

	start := time.Now()
	table, err := NewAdminClient(sock, 100*time.Millisecond).ReadVIPTable(context.Background())
	require.Error(t, err)
	assert.Empty(t, table)
	assert.Less(t, time.Since(start), 5*time.Second)
}

func TestAdminClient_NonOKStatus(t *testing.T) {
	fake := &fakeAdmin{}
	sock := fake.serve(t, func(w http.ResponseWriter, r *http.Request) { http.NotFound(w, r) })

	table, err := NewAdminClient(sock, time.Second).ReadVIPTable(context.Background())
	require.Error(t, err)
	assert.Empty(t, table)
}
