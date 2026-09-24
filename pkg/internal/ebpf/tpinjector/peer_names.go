// Copyright The OpenTelemetry Authors
// SPDX-License-Identifier: Apache-2.0

//go:build linux

package tpinjector // import "go.opentelemetry.io/obi/pkg/internal/ebpf/tpinjector"

import (
	"encoding/binary"
	"errors"
	"fmt"
	"net/netip"
)

// EXPERIMENTAL — TCP service-name propagation.

// peerAddrKeyLen is sizeof(svc_peer_addr_t) in bpf/maps/svc_name_by_peer.h:
// u32 ip4 (network order) + u16 port (host order) + 2 pad bytes. The key is read as raw
// bytes so the address needs no byte swap and nothing couples to the bpf2go type.
const peerAddrKeyLen = 8

var errPeerNamesNotLoaded = errors.New("svc_name_by_peer map is not loaded")

// PeerNames returns a snapshot of the service names learned from the kind-26 TCP option,
// keyed by the peer's "ip:port" (IPv4, e.g. "172.31.45.21:80"). It lets a caller join a
// Service Connect VIP to its backend addresses and from there to the backend's name.
// Entries with an unset or over-long name are left out.
func (p *Tracer) PeerNames() (map[string]string, error) {
	m := p.bpfObjects.SvcNameByPeer
	if m == nil {
		return nil, errPeerNamesNotLoaded
	}

	names := map[string]string{}
	var key [peerAddrKeyLen]byte
	var val [svcNameMaxLen + 1]byte
	it := m.Iterate()
	for it.Next(&key, &val) {
		if addr, name, ok := decodePeerName(key, val); ok {
			names[addr] = name
		}
	}
	if err := it.Err(); err != nil {
		return nil, fmt.Errorf("iterating svc_name_by_peer: %w", err)
	}
	return names, nil
}

// decodePeerName turns one raw svc_name_by_peer entry into ("ip:port", name).
func decodePeerName(key [peerAddrKeyLen]byte, val [svcNameMaxLen + 1]byte) (string, string, bool) {
	n := int(val[svcNameMaxLen])
	if n == 0 || n > svcNameMaxLen {
		return "", "", false
	}

	ip := netip.AddrFrom4([4]byte(key[0:4]))
	port := binary.NativeEndian.Uint16(key[4:6])
	return netip.AddrPortFrom(ip, port).String(), string(val[:n]), true
}
