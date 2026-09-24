// Copyright The OpenTelemetry Authors
// SPDX-License-Identifier: Apache-2.0

#pragma once

#include <bpfcore/vmlinux.h>
#include <bpfcore/bpf_helpers.h>

#include <common/map_sizing.h>
#include <common/pin_internal.h>
#include <maps/svc_peer_name_map.h> // svc_name_value_t

// EXPERIMENTAL — TCP service-name propagation.
//
// The same peer names as svc_peer_name_map, filed under the peer's address instead of the
// connection tuple. On ECS Service Connect the app talks to a loopback VIP and its Envoy
// sidecar opens the connection that carries the option, so the tuple never matches the
// app's socket. User space joins VIP -> Envoy backend set -> this map instead.
typedef struct svc_peer_addr {
    u32 ip4;  // network order, as skops->remote_ip4
    u16 port; // host order, bpf_ntohl(skops->remote_port) as in the tuple key
    u8 _pad[2];
} svc_peer_addr_t;

struct {
    __uint(type, BPF_MAP_TYPE_LRU_HASH);
    __type(key, svc_peer_addr_t);
    __type(value, svc_name_value_t);
    __uint(max_entries, MAX_CONCURRENT_REQUESTS);
    __uint(pinning, OBI_PIN_INTERNAL);
} svc_name_by_peer SEC(".maps");
