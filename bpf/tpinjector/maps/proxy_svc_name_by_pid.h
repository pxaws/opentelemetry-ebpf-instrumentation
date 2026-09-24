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
// The service.name a Service Connect Envoy sidecar borrows from the one app in its
// task, keyed like svc_name_by_pid by host PID (tgid). Kept apart from
// svc_name_by_pid because a borrowed name may only be stamped toward a non-loopback
// peer (see proxy_stamp_policy.h), so no write order in user space can open a
// self-stamp window. Written and deleted from user space by the tpinjector reconcile
// loop, which tracks each pid's start time so a reused pid never inherits a name.
struct {
    __uint(type, BPF_MAP_TYPE_LRU_HASH);
    __type(key, u32); // host PID (tgid)
    __type(value, svc_name_value_t);
    __uint(max_entries, MAX_CONCURRENT_REQUESTS);
    __uint(pinning, OBI_PIN_INTERNAL);
} proxy_svc_name_by_pid SEC(".maps");
