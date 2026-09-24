// Copyright The OpenTelemetry Authors
// SPDX-License-Identifier: Apache-2.0

#pragma once

#include <bpfcore/vmlinux.h>

// EXPERIMENTAL — TCP service-name propagation.
//
// Whether a peer's learned name may be filed under its address in svc_name_by_peer.
// Loopback peers collide across network namespaces (every task has its own 127.0.0.1)
// and are never Service Connect backends. 0.0.0.0 is what remote_ip4 reads on a pure
// IPv6 socket, so it names no peer.
//
// remote_ip4 is in network order, as skops->remote_ip4 stores it. Reading the first
// octet from memory keeps the check independent of host byte order.
static __always_inline bool svc_peer_addr_learnable(const u32 remote_ip4) {
    const u8 *octets = (const u8 *)&remote_ip4;
    const u8 loopback_net = 127;

    if (remote_ip4 == 0) {
        return false;
    }

    return octets[0] != loopback_net;
}
