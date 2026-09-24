// Copyright The OpenTelemetry Authors
// SPDX-License-Identifier: Apache-2.0

//go:build linux

package tpinjector

import (
	"encoding/binary"
	"testing"

	"github.com/stretchr/testify/assert"
)

// rawPeerKey lays out svc_peer_addr_t by hand: the address as wire bytes (network order,
// as skops->remote_ip4 stores it) and the port as a host-order u16 (bpf_ntohl(remote_port)).
func rawPeerKey(ip [4]byte, port uint16) [peerAddrKeyLen]byte {
	var k [peerAddrKeyLen]byte
	copy(k[0:4], ip[:])
	binary.NativeEndian.PutUint16(k[4:6], port)
	return k
}

func rawName(name string, length uint8) [svcNameMaxLen + 1]byte {
	var v [svcNameMaxLen + 1]byte
	copy(v[:svcNameMaxLen], name)
	v[svcNameMaxLen] = length
	return v
}

func TestDecodePeerName(t *testing.T) {
	addr, name, ok := decodePeerName(rawPeerKey([4]byte{172, 31, 45, 21}, 80), rawName("inventory", 9))
	assert.True(t, ok)
	assert.Equal(t, "172.31.45.21:80", addr)
	assert.Equal(t, "inventory", name)
}

func TestDecodePeerName_HighPort(t *testing.T) {
	addr, _, ok := decodePeerName(rawPeerKey([4]byte{10, 0, 3, 7}, 8443), rawName("payment", 7))
	assert.True(t, ok)
	assert.Equal(t, "10.0.3.7:8443", addr)
}

func TestDecodePeerName_FullLengthName(t *testing.T) {
	full := "abcdefghijklmnopqrstuvwxy" // exactly svcNameMaxLen
	_, name, ok := decodePeerName(rawPeerKey([4]byte{172, 31, 0, 1}, 80), rawName(full, svcNameMaxLen))
	assert.True(t, ok)
	assert.Equal(t, full, name)
}

func TestDecodePeerName_SkipsInvalidLength(t *testing.T) {
	key := rawPeerKey([4]byte{172, 31, 45, 21}, 80)

	_, _, ok := decodePeerName(key, rawName("", 0))
	assert.False(t, ok, "length 0 means unset")

	_, _, ok = decodePeerName(key, rawName("inventory", svcNameMaxLen+1))
	assert.False(t, ok, "length over the wire limit is never trusted")
}
