// Copyright The OpenTelemetry Authors
// SPDX-License-Identifier: Apache-2.0

package scproxy // import "go.opentelemetry.io/obi/pkg/internal/scproxy"

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/netip"
	"slices"
	"strings"
	"time"

	"go.opentelemetry.io/obi/pkg/export/otel/otelcfg"
)

// DefaultAdminTimeout bounds one admin request. The socket is local, so
// anything slower means Envoy is stuck and the caller should move on.
const DefaultAdminTimeout = 2 * time.Second

const (
	// The host never resolves this name; the unix dialer ignores it.
	adminBaseURL = "http://envoy-admin"

	// No include_eds: the parser takes backends from /clusters, not from EDS.
	configDumpPath = "/config_dump"
	clustersPath   = "/clusters?format=json"

	// A measured sidecar dump is ~130 KB. The cap only stops a runaway body.
	maxAdminResponseBytes = 16 << 20

	outboundDirection = "OUTBOUND"
	ipv4HostPrefixLen = 32
)

// VIPTable maps a Service Connect VIP (the loopback address and port the app
// dials) to the backend addresses Envoy may forward it to. Backends are
// sorted and unique. A VIP whose cluster has no hosts maps to an empty slice.
//
// The backend set comes from /clusters, which lists the hosts Envoy's
// cluster manager currently holds. The EDS section of /config_dump is the last
// assignment received, which can differ from the live set during an update,
// so it is not used. /config_dump supplies only VIP → cluster.
type VIPTable map[netip.AddrPort][]netip.AddrPort

// AdminClient reads one Envoy's admin interface over its unix socket. It
// issues GET requests only: the admin API also has mutating POST endpoints
// (drain, quit, logging), and the agent must never reach them.
type AdminClient struct {
	client *http.Client
	log    *slog.Logger
}

// NewAdminClient returns a client for the admin socket at socketPath,
// typically Sidecar.AdminSocket.
func NewAdminClient(socketPath string, timeout time.Duration) *AdminClient {
	return &AdminClient{
		client: &http.Client{Transport: otelcfg.UnixTransport(socketPath), Timeout: timeout},
		log:    slog.With("component", "scproxy.AdminClient", "socket", socketPath),
	}
}

// ReadVIPTable fetches /config_dump and /clusters and joins them.
func (c *AdminClient) ReadVIPTable(ctx context.Context) (VIPTable, error) {
	dump, err := c.get(ctx, configDumpPath)
	if err != nil {
		return nil, err
	}

	clusters, err := c.get(ctx, clustersPath)
	if err != nil {
		return nil, err
	}

	table, err := ParseVIPTable(dump, clusters)
	if err != nil {
		return nil, err
	}

	c.log.Debug("read VIP table", "vips", len(table))
	return table, nil
}

func (c *AdminClient) get(ctx context.Context, path string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, adminBaseURL+path, http.NoBody)
	if err != nil {
		return nil, fmt.Errorf("building GET %s: %w", path, err)
	}

	resp, err := c.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("GET %s: %w", path, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("GET %s: status %d", path, resp.StatusCode)
	}

	body, err := io.ReadAll(io.LimitReader(resp.Body, maxAdminResponseBytes+1))
	if err != nil {
		return nil, fmt.Errorf("GET %s: reading body: %w", path, err)
	}
	if len(body) > maxAdminResponseBytes {
		return nil, fmt.Errorf("GET %s: body exceeds %d bytes", path, maxAdminResponseBytes)
	}
	return body, nil
}

// ParseVIPTable joins a /config_dump body with a /clusters?format=json body.
// Only filter chains of OUTBOUND listeners that match a /32 IPv4 prefix and a
// destination port become VIPs. Missing JSON fields read as zero values; only
// malformed JSON is an error, and then no table is returned.
func ParseVIPTable(configDump, clusters []byte) (VIPTable, error) {
	var dump configDumpDoc
	if err := json.Unmarshal(configDump, &dump); err != nil {
		return nil, fmt.Errorf("parsing config_dump: %w", err)
	}

	var status clustersDoc
	if err := json.Unmarshal(clusters, &status); err != nil {
		return nil, fmt.Errorf("parsing clusters: %w", err)
	}

	vipClusters := dump.vipClusters()
	hosts := status.hostsByCluster()

	table := make(VIPTable, len(vipClusters))
	for vip, clusterNames := range vipClusters {
		backends := []netip.AddrPort{}
		for _, name := range clusterNames {
			backends = append(backends, hosts[name]...)
		}
		slices.SortFunc(backends, netip.AddrPort.Compare)
		table[vip] = slices.Compact(backends)
	}
	return table, nil
}

// The types below model only the fields the join reads. The five config dump
// sections have disjoint field names, so one struct decodes any of them.

type configDumpDoc struct {
	Configs []configSection `json:"configs"`
}

type configSection struct {
	DynamicListeners    []dynamicListener `json:"dynamic_listeners"`
	StaticListeners     []staticListener  `json:"static_listeners"`
	DynamicRouteConfigs []routeConfigItem `json:"dynamic_route_configs"`
	StaticRouteConfigs  []routeConfigItem `json:"static_route_configs"`
}

type dynamicListener struct {
	ActiveState struct {
		Listener listener `json:"listener"`
	} `json:"active_state"`
}

type staticListener struct {
	Listener listener `json:"listener"`
}

type listener struct {
	TrafficDirection string        `json:"traffic_direction"`
	FilterChains     []filterChain `json:"filter_chains"`
}

type filterChain struct {
	Match struct {
		PrefixRanges []struct {
			AddressPrefix string `json:"address_prefix"`
			PrefixLen     uint32 `json:"prefix_len"`
		} `json:"prefix_ranges"`
		DestinationPort uint16 `json:"destination_port"`
	} `json:"filter_chain_match"`
	Filters []networkFilter `json:"filters"`
}

// networkFilter covers the two terminal filters Service Connect uses: the
// HTTP connection manager (route config by RDS name or inline) and TCP proxy
// (cluster or weighted clusters directly).
type networkFilter struct {
	TypedConfig struct {
		RDS struct {
			RouteConfigName string `json:"route_config_name"`
		} `json:"rds"`
		RouteConfig      *routeConfig     `json:"route_config"`
		Cluster          string           `json:"cluster"`
		WeightedClusters weightedClusters `json:"weighted_clusters"`
	} `json:"typed_config"`
}

type routeConfigItem struct {
	RouteConfig routeConfig `json:"route_config"`
}

type routeConfig struct {
	Name         string `json:"name"`
	VirtualHosts []struct {
		Routes []struct {
			Route struct {
				Cluster          string           `json:"cluster"`
				WeightedClusters weightedClusters `json:"weighted_clusters"`
			} `json:"route"`
		} `json:"routes"`
	} `json:"virtual_hosts"`
}

type weightedClusters struct {
	Clusters []struct {
		Name string `json:"name"`
	} `json:"clusters"`
}

func (w weightedClusters) names() []string {
	names := make([]string, 0, len(w.Clusters))
	for _, c := range w.Clusters {
		names = append(names, c.Name)
	}
	return names
}

// clusters returns every cluster any route of the config can pick. Service
// Connect adds a blue/green test route next to the default one; both count,
// because the joiner must accept any backend the VIP can reach.
func (r routeConfig) clusters() []string {
	var names []string
	for _, vh := range r.VirtualHosts {
		for _, rt := range vh.Routes {
			if rt.Route.Cluster != "" {
				names = append(names, rt.Route.Cluster)
			}
			names = append(names, rt.Route.WeightedClusters.names()...)
		}
	}
	return names
}

func (d configDumpDoc) listeners() []listener {
	var out []listener
	for _, s := range d.Configs {
		for _, l := range s.DynamicListeners {
			out = append(out, l.ActiveState.Listener)
		}
		for _, l := range s.StaticListeners {
			out = append(out, l.Listener)
		}
	}
	return out
}

func (d configDumpDoc) routeConfigs() map[string]routeConfig {
	out := map[string]routeConfig{}
	for _, s := range d.Configs {
		for _, items := range [][]routeConfigItem{s.DynamicRouteConfigs, s.StaticRouteConfigs} {
			for _, item := range items {
				out[item.RouteConfig.Name] = item.RouteConfig
			}
		}
	}
	return out
}

func (d configDumpDoc) vipClusters() map[netip.AddrPort][]string {
	routes := d.routeConfigs()
	out := map[netip.AddrPort][]string{}

	for _, l := range d.listeners() {
		if l.TrafficDirection != outboundDirection {
			continue
		}
		for _, fc := range l.FilterChains {
			vips := fc.vips()
			if len(vips) == 0 {
				continue
			}
			clusters := fc.clusters(routes)
			for _, vip := range vips {
				out[vip] = append(out[vip], clusters...)
			}
		}
	}
	return out
}

func (fc filterChain) vips() []netip.AddrPort {
	if fc.Match.DestinationPort == 0 {
		return nil
	}

	var out []netip.AddrPort
	for _, pr := range fc.Match.PrefixRanges {
		if pr.PrefixLen != ipv4HostPrefixLen {
			continue
		}
		addr, err := netip.ParseAddr(pr.AddressPrefix)
		if err != nil || !addr.Is4() {
			continue
		}
		out = append(out, netip.AddrPortFrom(addr, fc.Match.DestinationPort))
	}
	return out
}

func (fc filterChain) clusters(routes map[string]routeConfig) []string {
	var names []string
	for _, f := range fc.Filters {
		tc := f.TypedConfig
		if tc.Cluster != "" {
			names = append(names, tc.Cluster)
		}
		names = append(names, tc.WeightedClusters.names()...)

		if tc.RouteConfig != nil {
			names = append(names, tc.RouteConfig.clusters()...)
		}
		if rc, ok := routes[tc.RDS.RouteConfigName]; ok && tc.RDS.RouteConfigName != "" {
			names = append(names, rc.clusters()...)
		}
	}
	return names
}

type clustersDoc struct {
	ClusterStatuses []struct {
		Name         string `json:"name"`
		HostStatuses []struct {
			Address socketAddressDoc `json:"address"`
		} `json:"host_statuses"`
	} `json:"cluster_statuses"`
}

type socketAddressDoc struct {
	SocketAddress struct {
		Address   string `json:"address"`
		PortValue uint16 `json:"port_value"`
	} `json:"socket_address"`
}

func (s socketAddressDoc) addrPort() (netip.AddrPort, error) {
	addr, err := netip.ParseAddr(strings.TrimSpace(s.SocketAddress.Address))
	if err != nil {
		return netip.AddrPort{}, err
	}
	if s.SocketAddress.PortValue == 0 {
		return netip.AddrPort{}, errors.New("missing port")
	}
	return netip.AddrPortFrom(addr, s.SocketAddress.PortValue), nil
}

// hostsByCluster skips hosts without an IP socket address (a pipe or a DNS
// name); the joiner matches on IP:port only.
func (c clustersDoc) hostsByCluster() map[string][]netip.AddrPort {
	out := map[string][]netip.AddrPort{}
	for _, cs := range c.ClusterStatuses {
		for _, hs := range cs.HostStatuses {
			ap, err := hs.Address.addrPort()
			if err != nil {
				continue
			}
			out[cs.Name] = append(out[cs.Name], ap)
		}
	}
	return out
}
