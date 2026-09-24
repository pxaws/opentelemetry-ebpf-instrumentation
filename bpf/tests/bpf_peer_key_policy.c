// Copyright The OpenTelemetry Authors
// SPDX-License-Identifier: Apache-2.0

#include <stdio.h>
#include <stdlib.h>
#include <string.h>

#include <tpinjector/peer_key_policy.h>

#include "test_ip4.h"

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
