// Copyright The OpenTelemetry Authors
// SPDX-License-Identifier: Apache-2.0

#include <stdio.h>
#include <stdlib.h>
#include <string.h>

#include <tpinjector/peer_key_policy.h>

// skops->remote_ip4 holds the address in network order, so the test builds it from the
// dotted-quad bytes in wire order rather than from a host-order integer.
static u32 ip4(u8 a, u8 b, u8 c, u8 d) {
    const u8 bytes[4] = {a, b, c, d};
    u32 v;
    memcpy(&v, bytes, sizeof(v));
    return v;
}

static void expect(u32 remote_ip4, bool want, const char *msg) {
    const bool got = svc_peer_addr_learnable(remote_ip4);
    if (got != want) {
        fprintf(stderr, "FAIL: %s (want %d, got %d)\n", msg, want, got);
        exit(1);
    }
}

int main(void) {
    expect(ip4(172, 31, 45, 21), true, "VPC backend is kept");
    expect(ip4(10, 0, 0, 1), true, "private backend is kept");

    expect(ip4(127, 0, 0, 1), false, "loopback is skipped");
    expect(ip4(127, 255, 0, 1), false, "Service Connect VIP range is skipped");
    expect(ip4(0, 0, 0, 0), false, "unspecified (pure IPv6 socket) is skipped");

    // a host-order reading would see 1.0.0.127 as loopback and 127.0.0.1 as not
    expect(ip4(1, 0, 0, 127), true, "last octet 127 is not loopback");

    printf("OK: %s\n", __FILE__);
    return 0;
}
