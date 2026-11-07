package trafficcontrol

import (
	"context"
	"net"
	"net/netip"
	"slices"
	"sync/atomic"
	"time"

	"github.com/sagernet/sing-box/adapter"
	C "github.com/sagernet/sing-box/constant"
	"github.com/sagernet/sing-tun"
	"github.com/sagernet/sing/common"
	"github.com/sagernet/sing/common/bufio"
	N "github.com/sagernet/sing/common/network"

	"github.com/gofrs/uuid/v5"
)

type TrackerMetadata struct {
	ID                  uuid.UUID
	Metadata            adapter.InboundContext
	CreatedAt           time.Time
	ClosedAt            time.Time
	Upload              *atomic.Int64
	Download            *atomic.Int64
	Chain               []string
	Rule                adapter.Rule
	Outbound            string
	OutboundType        string
	ResolvedDestination *atomic.Pointer[netip.Addr]
	OutboundDestination *atomic.Pointer[netip.Addr]
}

func (m *TrackerMetadata) DestinationAddr() netip.Addr {
	if m.Metadata.Destination.Addr.IsValid() {
		return m.Metadata.Destination.Addr.Unmap()
	}
	if m.ResolvedDestination != nil {
		if addr := m.ResolvedDestination.Load(); addr != nil && addr.IsValid() {
			return addr.Unmap()
		}
	}
	if len(m.Metadata.DestinationAddresses) > 0 && m.Metadata.DestinationAddresses[0].IsValid() {
		return m.Metadata.DestinationAddresses[0].Unmap()
	}
	return netip.Addr{}
}

func (m *TrackerMetadata) OutboundAddr() netip.Addr {
	if m.OutboundDestination != nil {
		if addr := m.OutboundDestination.Load(); addr != nil && addr.IsValid() {
			return addr.Unmap()
		}
	}
	return netip.Addr{}
}

func (m *TrackerMetadata) RecordRemoteAddr(addr netip.Addr) {
	if !addr.IsValid() {
		return
	}
	addrCopy := addr.Unmap()
	if m.OutboundType == C.TypeDirect || m.OutboundType == "" {
		if m.ResolvedDestination != nil {
			m.ResolvedDestination.Store(&addrCopy)
		}
	} else {
		if m.OutboundDestination != nil {
			m.OutboundDestination.Store(&addrCopy)
		}
	}
}

func (m *TrackerMetadata) RecordResolvedAddr(addr netip.Addr) {
	if !addr.IsValid() || m.ResolvedDestination == nil {
		return
	}
	addrCopy := addr.Unmap()
	m.ResolvedDestination.Store(&addrCopy)
}

type Tracker interface {
	Metadata() *TrackerMetadata
	Close() error
}

func (m *Manager) RoutedConnection(ctx context.Context, conn net.Conn, metadata adapter.InboundContext, matchedRule adapter.Rule, matchOutbound adapter.Outbound) net.Conn {
	upload := new(atomic.Int64)
	download := new(atomic.Int64)
	tracker := &connTracker{
		ExtendedConn: bufio.NewInt64CounterConn(conn, []*atomic.Int64{upload}, []*atomic.Int64{download}),
		metadata:     m.newTrackerMetadata(metadata, matchedRule, matchOutbound, upload, download),
		manager:      m,
	}
	m.join(tracker)
	return tracker
}

func (m *Manager) RoutedPacketConnection(ctx context.Context, conn N.PacketConn, metadata adapter.InboundContext, matchedRule adapter.Rule, matchOutbound adapter.Outbound) N.PacketConn {
	upload := new(atomic.Int64)
	download := new(atomic.Int64)
	tracker := &packetConnTracker{
		PacketConn: bufio.NewInt64CounterPacketConn(conn, []*atomic.Int64{upload}, nil, []*atomic.Int64{download}, nil),
		metadata:   m.newTrackerMetadata(metadata, matchedRule, matchOutbound, upload, download),
		manager:    m,
	}
	m.join(tracker)
	return tracker
}

func (m *Manager) RoutedFlow(ctx context.Context, metadata adapter.InboundContext, matchedRule adapter.Rule, matchOutbound adapter.Outbound) tun.FlowTracker {
	return &flowTracker{
		metadata: m.newTrackerMetadata(metadata, matchedRule, matchOutbound, new(atomic.Int64), new(atomic.Int64)),
		manager:  m,
	}
}

func (m *Manager) newTrackerMetadata(metadata adapter.InboundContext, matchedRule adapter.Rule, matchOutbound adapter.Outbound, upload *atomic.Int64, download *atomic.Int64) TrackerMetadata {
	id, _ := uuid.NewV4()
	chain := common.Map(metadata.OutboundChain, adapter.Outbound.Tag)
	slices.Reverse(chain)
	outbound := metadata.OutboundChain[len(metadata.OutboundChain)-1]
	resolvedDest := new(atomic.Pointer[netip.Addr])
	if len(metadata.DestinationAddresses) > 0 && metadata.DestinationAddresses[0].IsValid() {
		addrCopy := metadata.DestinationAddresses[0].Unmap()
		resolvedDest.Store(&addrCopy)
	}
	return TrackerMetadata{
		ID:                  id,
		Metadata:            metadata,
		CreatedAt:           time.Now(),
		Upload:              upload,
		Download:            download,
		Chain:               chain,
		Rule:                matchedRule,
		Outbound:            outbound.Tag(),
		OutboundType:        outbound.Type(),
		ResolvedDestination: resolvedDest,
		OutboundDestination: new(atomic.Pointer[netip.Addr]),
	}
}

type connTracker struct {
	N.ExtendedConn
	metadata TrackerMetadata
	manager  *Manager
}

func (t *connTracker) Metadata() *TrackerMetadata {
	return &t.metadata
}

func (t *connTracker) UpdateDestinationAddress(addr netip.Addr, resolvedAddr netip.Addr) {
	if resolvedAddr.IsValid() {
		t.metadata.RecordResolvedAddr(resolvedAddr)
	}
	if addr.IsValid() {
		t.metadata.RecordRemoteAddr(addr)
	}
}

func (t *connTracker) Close() error {
	t.manager.leave(t)
	return t.ExtendedConn.Close()
}

func (t *connTracker) Upstream() any {
	return t.ExtendedConn
}

func (t *connTracker) ReaderReplaceable() bool {
	return true
}

func (t *connTracker) WriterReplaceable() bool {
	return true
}

var (
	_ Tracker         = (*flowTracker)(nil)
	_ tun.FlowTracker = (*flowTracker)(nil)
)

type flowTracker struct {
	metadata TrackerMetadata
	manager  *Manager
	handle   tun.FlowHandle
}

func (t *flowTracker) Metadata() *TrackerMetadata {
	return &t.metadata
}

func (t *flowTracker) AttachFlow(handle tun.FlowHandle) {
	t.handle = handle
	t.manager.join(t)
}

func (t *flowTracker) CountForward(n int) {
	t.metadata.Upload.Add(int64(n))
}

func (t *flowTracker) CountReverse(n int) {
	t.metadata.Download.Add(int64(n))
}

func (t *flowTracker) FlowEstablished() {
}

func (t *flowTracker) CloseFlow(reason tun.FlowCloseReason) {
	t.manager.leave(t)
}

func (t *flowTracker) Close() error {
	handle := t.handle
	if handle != nil {
		handle.CloseFlow()
	} else {
		t.manager.leave(t)
	}
	return nil
}

type packetConnTracker struct {
	N.PacketConn
	metadata TrackerMetadata
	manager  *Manager
}

func (t *packetConnTracker) Metadata() *TrackerMetadata {
	return &t.metadata
}

func (t *packetConnTracker) UpdateDestinationAddress(addr netip.Addr, resolvedAddr netip.Addr) {
	if resolvedAddr.IsValid() {
		t.metadata.RecordResolvedAddr(resolvedAddr)
	}
	if addr.IsValid() {
		t.metadata.RecordRemoteAddr(addr)
	}
}

func (t *packetConnTracker) Close() error {
	t.manager.leave(t)
	return t.PacketConn.Close()
}

func (t *packetConnTracker) Upstream() any {
	return t.PacketConn
}

func (t *packetConnTracker) ReaderReplaceable() bool {
	return true
}

func (t *packetConnTracker) WriterReplaceable() bool {
	return true
}
