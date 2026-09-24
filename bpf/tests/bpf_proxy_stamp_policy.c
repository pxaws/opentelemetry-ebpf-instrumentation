// Copyright The OpenTelemetry Authors
// SPDX-License-Identifier: Apache-2.0

#include <stdio.h>
#include <stdlib.h>
#include <string.h>

#include <tpinjector/proxy_stamp_policy.h>

// msg->remote_ip4 holds the address in network order, so the test builds it from the
// dotted-quad bytes in wire order rather than from a host-order integer.
static u32 ip4(u8 a, u8 b, u8 c, u8 d) {
    const u8 bytes[4] = {a, b, c, d};
    u32 v;
    memcpy(&v, bytes, sizeof(v));
    return v;
}

static void
expect(bool has_direct, bool has_proxy, u32 remote_ip4, svc_name_source_t want, const char *msg) {
    const svc_name_source_t got = svc_name_source(has_direct, has_proxy, remote_ip4);
    if (got != want) {
        fprintf(stderr, "FAIL: %s (want %d, got %d)\n", msg, want, got);
        exit(1);
    }
}

int main(void) {
    const u32 task_ip = ip4(172, 31, 45, 21);

    expect(true, false, task_ip, k_svc_name_direct, "app with a direct name stamps");
    expect(true, false, ip4(127, 0, 0, 1), k_svc_name_direct, "direct name ignores loopback");
    expect(true, false, 0, k_svc_name_direct, "direct name ignores 0.0.0.0");
    expect(true, true, ip4(127, 0, 0, 1), k_svc_name_direct, "direct name wins over proxy");

    expect(false, true, task_ip, k_svc_name_proxy, "proxy stamps toward a task address");

    // self-stamp trap: leg 1 reaches the Envoy egress listener over loopback
    expect(false, true, ip4(127, 0, 0, 1), k_svc_name_none, "proxy skips 127.0.0.1");
    expect(false, true, ip4(127, 255, 0, 1), k_svc_name_none, "proxy skips the VIP range");
    expect(false, true, 0, k_svc_name_none, "proxy skips 0.0.0.0");

    expect(false, false, task_ip, k_svc_name_none, "pid in neither map does not stamp");

    // a host-order reading would see 1.0.0.127 as loopback and 127.0.0.1 as not
    expect(false, true, ip4(1, 0, 0, 127), k_svc_name_proxy, "last octet 127 is not loopback");

    printf("OK: %s\n", __FILE__);
    return 0;
}
