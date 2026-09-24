// Copyright The OpenTelemetry Authors
// SPDX-License-Identifier: Apache-2.0

#pragma once

#include <string.h>

#include <bpfcore/vmlinux.h>

// skops->remote_ip4 and msg->remote_ip4 hold the address in network order, so tests
// build it from the dotted-quad bytes in wire order rather than from a host-order integer.
static inline u32 ip4(u8 a, u8 b, u8 c, u8 d) {
    const u8 bytes[4] = {a, b, c, d};
    u32 v;
    memcpy(&v, bytes, sizeof(v));
    return v;
}
