// Copyright The OpenTelemetry Authors
// SPDX-License-Identifier: Apache-2.0

package transform // import "go.opentelemetry.io/obi/pkg/transform"

import (
	"context"
	"log/slog"
	"math"
	"net/netip"
	"time"

	"github.com/hashicorp/golang-lru/v2/expirable"

	"go.opentelemetry.io/obi/pkg/appolly/app"
	"go.opentelemetry.io/obi/pkg/appolly/app/request"
	"go.opentelemetry.io/obi/pkg/internal/scproxy"
	"go.opentelemetry.io/obi/pkg/pipe/global"
	"go.opentelemetry.io/obi/pkg/pipe/msg"
	"go.opentelemetry.io/obi/pkg/pipe/swarm"
	"go.opentelemetry.io/obi/pkg/pipe/swarm/swarms"
)

// EXPERIMENTAL (PoC) — Service Connect peer names.

// serviceConnectVIPs is the loopback range ECS Service Connect assigns to its VIPs.
var serviceConnectVIPs = netip.MustParsePrefix("127.255.0.0/16")

const (
	netnsCacheLen = 1024
	// bounds how long a recycled pid can be mapped to its previous owner's netns
	netnsCacheTTL = time.Minute
)

func scpnlog() *slog.Logger {
	return slog.With("component", "transform.SCPeerName")
}

// SCPeerNameProvider sets PeerServiceName on HTTP client spans sent to a Service Connect
// VIP, from the callee name the sidecar's current backends agree on. It only fills an
// empty PeerServiceName and never guesses: a span it can't resolve passes unchanged.
func SCPeerNameProvider(ctxInfo *global.ContextInfo, cfg *scproxy.SCProxyConfig,
	input, output *msg.Queue[[]request.Span],
) swarm.InstanceFunc {
	return func(_ context.Context) (swarm.RunFunc, error) {
		if cfg == nil || !cfg.Enabled {
			return swarm.Bypass(input, output)
		}

		joiner := scproxy.NewJoiner(cfg, ctxInfo.SCPeerNames)
		namer := newSCPeerNamer(cfg.ProcRootOrDefault(), joiner.Lookup)
		in := input.Subscribe(msg.SubscriberName("transform.SCPeerName"))
		return func(ctx context.Context) {
			// output channel must be closed so later stages in the pipeline can finish in cascade
			defer output.Close()
			go joiner.Run(ctx)
			swarms.ForEachInput(ctx, in, scpnlog().Debug, func(spans []request.Span) {
				for i := range spans {
					namer.decorate(&spans[i])
				}
				output.SendCtx(ctx, spans)
			})
		}, nil
	}
}

type scPeerNamer struct {
	procRoot   string
	lookup     func(netns uint64, vip netip.AddrPort) (string, bool)
	netnsByPID *expirable.LRU[app.PID, uint64]
}

func newSCPeerNamer(procRoot string, lookup func(uint64, netip.AddrPort) (string, bool)) *scPeerNamer {
	return &scPeerNamer{
		procRoot:   procRoot,
		lookup:     lookup,
		netnsByPID: expirable.NewLRU[app.PID, uint64](netnsCacheLen, nil, netnsCacheTTL),
	}
}

func (n *scPeerNamer) decorate(span *request.Span) {
	if span.Type != request.EventTypeHTTPClient || span.PeerServiceName != "" {
		return
	}
	vip, ok := serviceConnectVIP(span)
	if !ok {
		return
	}
	netns, ok := n.netns(span.Pid.HostPID)
	if !ok {
		return
	}
	if name, ok := n.lookup(netns, vip); ok {
		span.PeerServiceName = name
	}
}

func serviceConnectVIP(span *request.Span) (netip.AddrPort, bool) {
	addr, err := netip.ParseAddr(span.Host)
	if err != nil || !serviceConnectVIPs.Contains(addr) {
		return netip.AddrPort{}, false
	}
	if span.HostPort <= 0 || span.HostPort > math.MaxUint16 {
		return netip.AddrPort{}, false
	}
	return netip.AddrPortFrom(addr, uint16(span.HostPort)), true
}

// netns is keyed by host pid, not span.Pid.Namespace, which is the pid namespace. A pid
// that has already exited can't be resolved and its span stays unnamed.
func (n *scPeerNamer) netns(pid app.PID) (uint64, bool) {
	if netns, ok := n.netnsByPID.Get(pid); ok {
		return netns, true
	}
	netns, err := scproxy.ReadNetNS(n.procRoot, int32(pid))
	if err != nil {
		return 0, false
	}
	n.netnsByPID.Add(pid, netns)
	return netns, true
}
