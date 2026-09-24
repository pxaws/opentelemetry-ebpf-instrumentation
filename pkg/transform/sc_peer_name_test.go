// Copyright The OpenTelemetry Authors
// SPDX-License-Identifier: Apache-2.0

package transform

import (
	"net/netip"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.opentelemetry.io/obi/pkg/appolly/app"
	"go.opentelemetry.io/obi/pkg/appolly/app/request"
	attr "go.opentelemetry.io/obi/pkg/export/attributes/names"
	"go.opentelemetry.io/obi/pkg/internal/scproxy"
	"go.opentelemetry.io/obi/pkg/internal/testutil"
	"go.opentelemetry.io/obi/pkg/pipe/global"
	"go.opentelemetry.io/obi/pkg/pipe/msg"
)

const (
	testNetNS   = uint64(4026532553)
	testCallPID = app.PID(4242)
)

// fakeProc builds a /proc with one pid in network namespace netns.
func fakeProc(t *testing.T, pid app.PID, netns uint64) string {
	t.Helper()
	root := t.TempDir()
	nsDir := filepath.Join(root, strconv.Itoa(int(pid)), "ns")
	require.NoError(t, os.MkdirAll(nsDir, 0o755))
	require.NoError(t, os.Symlink("net:["+strconv.FormatUint(netns, 10)+"]", filepath.Join(nsDir, "net")))
	return root
}

func fakeLookup(netns uint64, vip netip.AddrPort) (string, bool) {
	if netns == testNetNS && vip == netip.MustParseAddrPort("127.255.0.1:8080") {
		return "blog-service", true
	}
	return "", false
}

func testPeerNamer(t *testing.T) *scPeerNamer {
	t.Helper()
	return newSCPeerNamer(fakeProc(t, testCallPID, testNetNS), fakeLookup)
}

func vipClientSpan() request.Span {
	return request.Span{
		Type:     request.EventTypeHTTPClient,
		Host:     "127.255.0.1",
		HostPort: 8080,
		Pid:      request.PidInfo{HostPID: testCallPID},
	}
}

func TestSCPeerName_VIPClientSpanGetsCalleeName(t *testing.T) {
	span := vipClientSpan()
	testPeerNamer(t).decorate(&span)
	assert.Equal(t, "blog-service", span.PeerServiceName)
}

func TestSCPeerName_ExistingPeerServiceNameIsKept(t *testing.T) {
	span := vipClientSpan()
	span.PeerServiceName = "from-tcp-option"
	testPeerNamer(t).decorate(&span)
	assert.Equal(t, "from-tcp-option", span.PeerServiceName)
}

func TestSCPeerName_NonVIPDestinationIsUntouched(t *testing.T) {
	span := vipClientSpan()
	span.Host = "172.31.45.21"
	span.HostPort = 80
	want := span
	testPeerNamer(t).decorate(&span)
	assert.Equal(t, want, span)
}

func TestSCPeerName_ServerSpanIsUntouched(t *testing.T) {
	span := vipClientSpan()
	span.Type = request.EventTypeHTTP
	want := span
	testPeerNamer(t).decorate(&span)
	assert.Equal(t, want, span)
}

func TestSCPeerName_UnknownVIPGetsNoName(t *testing.T) {
	span := vipClientSpan()
	span.Host = "127.255.0.2"
	testPeerNamer(t).decorate(&span)
	assert.Empty(t, span.PeerServiceName)
}

func TestSCPeerName_GonePIDGetsNoName(t *testing.T) {
	span := vipClientSpan()
	span.Pid.HostPID = testCallPID + 1
	testPeerNamer(t).decorate(&span)
	assert.Empty(t, span.PeerServiceName)
}

func TestSCPeerName_NameReachesHTTPClientMetricAttribute(t *testing.T) {
	span := vipClientSpan()
	testPeerNamer(t).decorate(&span)

	getter, ok := request.SpanOTELGetters(request.UnresolvedNames{})(attr.PeerServiceName)
	require.True(t, ok)
	kv := getter(&span)
	assert.Equal(t, string(attr.PeerServiceName), string(kv.Key))
	assert.Equal(t, "blog-service", kv.Value.AsString())
}

func runSCPeerNameProvider(t *testing.T, cfg *scproxy.SCProxyConfig, spans []request.Span) []request.Span {
	t.Helper()
	input := msg.NewQueue[[]request.Span](msg.ChannelBufferLen(10))
	output := msg.NewQueue[[]request.Span](msg.ChannelBufferLen(10))
	outCh := output.Subscribe()

	run, err := SCPeerNameProvider(&global.ContextInfo{}, cfg, input, output)(t.Context())
	require.NoError(t, err)
	go run(t.Context())

	input.Send(spans)
	return testutil.ReadChannel(t, outCh, 5*time.Second)
}

func TestSCPeerNameProvider_DisabledBypasses(t *testing.T) {
	out := runSCPeerNameProvider(t, &scproxy.SCProxyConfig{Enabled: false}, []request.Span{vipClientSpan()})
	require.Len(t, out, 1)
	assert.Empty(t, out[0].PeerServiceName)

	out = runSCPeerNameProvider(t, nil, []request.Span{vipClientSpan()})
	require.Len(t, out, 1)
	assert.Empty(t, out[0].PeerServiceName)
}

func TestSCPeerNameProvider_EnabledWithoutSidecarsPassesSpansThrough(t *testing.T) {
	cfg := &scproxy.SCProxyConfig{Enabled: true, Interval: time.Hour, ProcRoot: fakeProc(t, testCallPID, testNetNS)}
	out := runSCPeerNameProvider(t, cfg, []request.Span{vipClientSpan()})
	require.Len(t, out, 1)
	assert.Empty(t, out[0].PeerServiceName)
}
