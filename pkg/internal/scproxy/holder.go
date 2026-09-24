// Copyright The OpenTelemetry Authors
// SPDX-License-Identifier: Apache-2.0

package scproxy // import "go.opentelemetry.io/obi/pkg/internal/scproxy"

import "sync/atomic"

// PeerNameSource returns the service names learned per backend "ip:port".
// tpinjector.Tracer implements it.
type PeerNameSource interface {
	PeerNames() (map[string]string, error)
}

// PeerNameHolder late-binds the PeerNameSource. The pipeline that reads names is
// built before any tracer exists, and tpinjector loads lazily and may never load,
// so the holder can stay empty for the whole run. An empty or nil holder reports
// no names and no error.
type PeerNameHolder struct {
	src atomic.Pointer[PeerNameSource]
}

func (h *PeerNameHolder) Set(src PeerNameSource) {
	if h == nil {
		return
	}
	h.src.Store(&src)
}

func (h *PeerNameHolder) PeerNames() (map[string]string, error) {
	if h == nil {
		return nil, nil
	}
	src := h.src.Load()
	if src == nil {
		return nil, nil
	}
	return (*src).PeerNames()
}
