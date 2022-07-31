package tun

import (
	"context"
	"math"
	"net"
	"net/netip"

	"github.com/sagernet/sing/common/buf"
	M "github.com/sagernet/sing/common/metadata"
	N "github.com/sagernet/sing/common/network"
	"github.com/sagernet/sing/common/udpnat"

	gBuffer "gvisor.dev/gvisor/pkg/buffer"
	"gvisor.dev/gvisor/pkg/tcpip"
	"gvisor.dev/gvisor/pkg/tcpip/header"
	"gvisor.dev/gvisor/pkg/tcpip/stack"
)

type UDPForwarder struct {
	ctx     context.Context
	stack   *stack.Stack
	handler Handler
	udpNat  *udpnat.Service[netip.AddrPort]
}

func NewUDPForwarder(ctx context.Context, stack *stack.Stack, handler Handler, udpTimeout int64) *UDPForwarder {
	return &UDPForwarder{
		ctx:     ctx,
		stack:   stack,
		handler: handler,
		udpNat:  udpnat.New[netip.AddrPort](udpTimeout, nopErrorHandler{handler}),
	}
}

func (f *UDPForwarder) HandlePacket(id stack.TransportEndpointID, pkt *stack.PacketBuffer) bool {
	var upstreamMetadata M.Metadata
	upstreamMetadata.Source = M.SocksaddrFrom(M.AddrFromIP(net.IP(id.RemoteAddress)), id.RemotePort)
	upstreamMetadata.Destination = M.SocksaddrFrom(M.AddrFromIP(net.IP(id.LocalAddress)), id.LocalPort)

	f.udpNat.NewPacket(
		f.ctx,
		upstreamMetadata.Source.AddrPort(),
		buf.As(pkt.Data().AsRange().AsView()),
		upstreamMetadata,
		func(natConn N.PacketConn) N.PacketWriter {
			return &UDPBackWriter{f.stack, id.RemoteAddress, id.RemotePort}
		},
	)
	return true
}

type UDPBackWriter struct {
	stack      *stack.Stack
	source     tcpip.Address
	sourcePort uint16
}

func (w *UDPBackWriter) WritePacket(buffer *buf.Buffer, destination M.Socksaddr) error {
	defer buffer.Release()

	var netProto tcpip.NetworkProtocolNumber
	if destination.IsIPv4() {
		netProto = header.IPv4ProtocolNumber
	} else {
		netProto = header.IPv6ProtocolNumber
	}

	route, err := w.stack.FindRoute(
		defaultNIC,
		tcpip.Address(destination.Addr.AsSlice()),
		w.source,
		netProto,
		false,
	)
	if err != nil {
		return wrapStackError(err)
	}
	defer route.Release()

	packet := stack.NewPacketBuffer(stack.PacketBufferOptions{
		ReserveHeaderBytes: header.UDPMinimumSize + int(route.MaxHeaderLength()),
		Payload:            gBuffer.NewWithData(buffer.Bytes()),
	})
	defer packet.DecRef()

	packet.TransportProtocolNumber = header.UDPProtocolNumber
	udpHdr := header.UDP(packet.TransportHeader().Push(header.UDPMinimumSize))
	pLen := uint16(packet.Size())
	udpHdr.Encode(&header.UDPFields{
		SrcPort: destination.Port,
		DstPort: w.sourcePort,
		Length:  pLen,
	})

	if route.RequiresTXTransportChecksum() && netProto == header.IPv6ProtocolNumber {
		xsum := udpHdr.CalculateChecksum(header.ChecksumCombine(
			route.PseudoHeaderChecksum(header.UDPProtocolNumber, pLen),
			packet.Data().AsRange().Checksum(),
		))
		if xsum != math.MaxUint16 {
			xsum = ^xsum
		}
		udpHdr.SetChecksum(xsum)
	}

	err = route.WritePacket(stack.NetworkHeaderParams{
		Protocol: header.UDPProtocolNumber,
		TTL:      route.DefaultTTL(),
		TOS:      0,
	}, packet)

	if err != nil {
		route.Stats().UDP.PacketSendErrors.Increment()
		return wrapStackError(err)
	}

	route.Stats().UDP.PacketsSent.Increment()
	return nil
}

type nopErrorHandler struct {
	Handler
}

func (h nopErrorHandler) NewError(ctx context.Context, err error) {
}