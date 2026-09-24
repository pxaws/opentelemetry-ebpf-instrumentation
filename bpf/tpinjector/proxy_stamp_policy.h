// Copyright The OpenTelemetry Authors
// SPDX-License-Identifier: Apache-2.0

#pragma once

#include <bpfcore/vmlinux.h>

#include <tpinjector/peer_key_policy.h>

// EXPERIMENTAL — TCP service-name propagation.
//
// Which map supplies the name stamped on a server response. A process's own name
// (svc_name_by_pid) always wins. A Service Connect Envoy has none, so it borrows the
// name of the one app in its task (proxy_svc_name_by_pid), but only toward a peer in
// another network namespace. Envoy also accepts its own app's calls on the loopback
// egress listener; stamping there would hand each app its own name as the peer.
// The caller's Envoy always connects from a task address, never from loopback, so the
// same rule that keys learned peer names picks the ingress side.
typedef enum {
    k_svc_name_none = 0,
    k_svc_name_direct,
    k_svc_name_proxy,
} svc_name_source_t;

static __always_inline svc_name_source_t svc_name_source(const bool has_direct,
                                                         const bool has_proxy,
                                                         const u32 remote_ip4) {
    if (has_direct) {
        return k_svc_name_direct;
    }
    if (has_proxy && svc_peer_addr_learnable(remote_ip4)) {
        return k_svc_name_proxy;
    }

    return k_svc_name_none;
}
