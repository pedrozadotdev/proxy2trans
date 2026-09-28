package main

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
	"gopkg.in/yaml.v3"
)

const (
	proxyV2HeaderLen = 16
	maxUDPDatagram   = 65535
)

var proxyV2Signature = []byte{
	0x0d, 0x0a, 0x0d, 0x0a,
	0x00, 0x0d, 0x0a, 0x51,
	0x55, 0x49, 0x54, 0x0a,
}

// -----------------------------------------------------------------------------
// Logging
// -----------------------------------------------------------------------------

type LogLevel int

const (
	LogDebug LogLevel = iota
	LogInfo
	LogWarn
	LogError
)

var currentLogLevel = LogInfo

func parseLogLevel(value string) (LogLevel, error) {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "", "info":
		return LogInfo, nil
	case "debug":
		return LogDebug, nil
	case "warn", "warning":
		return LogWarn, nil
	case "error":
		return LogError, nil
	default:
		return LogInfo, fmt.Errorf(
			"invalid log_level %q: expected debug, info, warn, or error",
			value,
		)
	}
}

func setLogLevel(level LogLevel) {
	currentLogLevel = level
}

func logf(level LogLevel, label string, format string, args ...any) {
	if level < currentLogLevel {
		return
	}

	allArgs := append([]any{label}, args...)
	log.Printf("[%s] "+format, allArgs...)
}

func debugf(format string, args ...any) { logf(LogDebug, "DEBUG", format, args...) }
func infof(format string, args ...any)  { logf(LogInfo, "INFO", format, args...) }
func warnf(format string, args ...any)  { logf(LogWarn, "WARN", format, args...) }
func errorf(format string, args ...any) { logf(LogError, "ERROR", format, args...) }

// -----------------------------------------------------------------------------
// Configuration
// -----------------------------------------------------------------------------

type Config struct {
	LogLevel  string     `yaml:"log_level"`
	Listeners []Listener `yaml:"listeners"`
}

type Listener struct {
	Name        string
	Protocol    string
	Listen      string
	Target      string
	IdleTimeout time.Duration
}

func (l *Listener) UnmarshalYAML(value *yaml.Node) error {
	var raw struct {
		Name        string `yaml:"name"`
		Protocol    string `yaml:"protocol"`
		Listen      string `yaml:"listen"`
		Target      string `yaml:"target"`
		IdleTimeout string `yaml:"idle_timeout"`
	}

	if err := value.Decode(&raw); err != nil {
		return err
	}

	l.Name = strings.TrimSpace(raw.Name)
	l.Protocol = strings.ToLower(strings.TrimSpace(raw.Protocol))
	l.Listen = strings.TrimSpace(raw.Listen)
	l.Target = strings.TrimSpace(raw.Target)

	if raw.IdleTimeout != "" {
		d, err := time.ParseDuration(raw.IdleTimeout)
		if err != nil {
			return fmt.Errorf(
				"listener %q: invalid idle_timeout %q: %w",
				l.Name,
				raw.IdleTimeout,
				err,
			)
		}

		l.IdleTimeout = d
	} else {
		l.IdleTimeout = 2 * time.Minute
	}

	return nil
}

func loadConfig(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read config %q: %w", path, err)
	}

	var cfg Config

	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("parse config %q: %w", path, err)
	}

	level, err := parseLogLevel(cfg.LogLevel)
	if err != nil {
		return nil, err
	}

	switch level {
	case LogDebug:
		cfg.LogLevel = "debug"
	case LogInfo:
		cfg.LogLevel = "info"
	case LogWarn:
		cfg.LogLevel = "warn"
	case LogError:
		cfg.LogLevel = "error"
	}

	if len(cfg.Listeners) == 0 {
		return nil, errors.New("no listeners configured")
	}

	for i := range cfg.Listeners {
		l := &cfg.Listeners[i]

		if l.Name == "" {
			return nil, fmt.Errorf("listener %d has no name", i)
		}

		switch l.Protocol {
		case "tcp", "udp":
		default:
			return nil, fmt.Errorf(
				"listener %q: unsupported protocol %q",
				l.Name,
				l.Protocol,
			)
		}

		if l.Listen == "" {
			return nil, fmt.Errorf(
				"listener %q: listen address is empty",
				l.Name,
			)
		}

		if l.Target == "" {
			return nil, fmt.Errorf(
				"listener %q: target address is empty",
				l.Name,
			)
		}

		if l.IdleTimeout <= 0 {
			return nil, fmt.Errorf(
				"listener %q: idle_timeout must be > 0",
				l.Name,
			)
		}
	}

	return &cfg, nil
}

// -----------------------------------------------------------------------------
// PROXY Protocol v2
// -----------------------------------------------------------------------------

type ProxyInfo struct {
	Source      *net.UDPAddr
	Destination *net.UDPAddr
	HeaderLen   int
	Transport   byte
}

func parseProxyV2(data []byte) (*ProxyInfo, error) {
	if len(data) < proxyV2HeaderLen {
		return nil, errors.New("PROXY v2 header too short")
	}

	if !bytes.Equal(data[:12], proxyV2Signature) {
		return nil, errors.New("invalid PROXY v2 signature")
	}

	verCmd := data[12]

	version := verCmd >> 4
	command := verCmd & 0x0f

	if version != 0x2 {
		return nil, fmt.Errorf(
			"unsupported PROXY protocol version: %d",
			version,
		)
	}

	// 0x0 = LOCAL
	// 0x1 = PROXY
	if command != 0x1 {
		return nil, fmt.Errorf(
			"unsupported PROXY v2 command: %d",
			command,
		)
	}

	famProto := data[13]

	family := famProto >> 4
	transport := famProto & 0x0f

	length := int(binary.BigEndian.Uint16(data[14:16]))

	totalLen := proxyV2HeaderLen + length

	if len(data) < totalLen {
		return nil, fmt.Errorf(
			"incomplete PROXY v2 header: need %d bytes, have %d",
			totalLen,
			len(data),
		)
	}

	switch family {
	case 0x1: // AF_INET / IPv4
		if length < 12 {
			return nil, fmt.Errorf(
				"invalid IPv4 PROXY v2 address length: %d",
				length,
			)
		}

		addr := data[16 : 16+12]

		srcIP := net.IPv4(
			addr[0],
			addr[1],
			addr[2],
			addr[3],
		)

		dstIP := net.IPv4(
			addr[4],
			addr[5],
			addr[6],
			addr[7],
		)

		srcPort := int(binary.BigEndian.Uint16(addr[8:10]))
		dstPort := int(binary.BigEndian.Uint16(addr[10:12]))

		return &ProxyInfo{
			Source: &net.UDPAddr{
				IP:   srcIP,
				Port: srcPort,
			},
			Destination: &net.UDPAddr{
				IP:   dstIP,
				Port: dstPort,
			},
			HeaderLen: totalLen,
			Transport: transport,
		}, nil

	default:
		return nil, fmt.Errorf(
			"unsupported PROXY v2 address family: 0x%x",
			family,
		)
	}
}

func readTCPProxyHeader(conn net.Conn) (*ProxyInfo, error) {
	fixed := make([]byte, proxyV2HeaderLen)

	if _, err := io.ReadFull(conn, fixed); err != nil {
		return nil, fmt.Errorf(
			"read PROXY v2 fixed header: %w",
			err,
		)
	}

	if !bytes.Equal(fixed[:12], proxyV2Signature) {
		return nil, errors.New("invalid PROXY v2 signature")
	}

	length := int(binary.BigEndian.Uint16(fixed[14:16]))

	body := make([]byte, length)

	if length > 0 {
		if _, err := io.ReadFull(conn, body); err != nil {
			return nil, fmt.Errorf(
				"read PROXY v2 body: %w",
				err,
			)
		}
	}

	full := make([]byte, 0, len(fixed)+len(body))
	full = append(full, fixed...)
	full = append(full, body...)

	info, err := parseProxyV2(full)
	if err != nil {
		return nil, err
	}

	// PROXY v2 transport protocol:
	// 0x1 = STREAM
	if info.Transport != 0x1 {
		return nil, fmt.Errorf(
			"TCP listener received PROXY v2 transport 0x%x instead of STREAM",
			info.Transport,
		)
	}

	return info, nil
}

// -----------------------------------------------------------------------------
// Transparent sockets
// -----------------------------------------------------------------------------

func setIPTransparent(fd uintptr) error {
	return unix.SetsockoptInt(
		int(fd),
		unix.SOL_IP,
		unix.IP_TRANSPARENT,
		1,
	)
}

// TCP preserves the complete original source IP:port from PROXY v2.
//
// IP_TRANSPARENT is enabled before Go binds the LocalAddr. SO_REUSEADDR helps
// with rapid reconnects that reuse the same client tuple, although the kernel
// can still reject an exact tuple while an incompatible socket is active.
func transparentDialTCP(
	source *net.TCPAddr,
	target *net.TCPAddr,
) (*net.TCPConn, error) {
	if source == nil {
		return nil, errors.New("nil TCP source address")
	}

	ip4 := source.IP.To4()
	if ip4 == nil {
		return nil, errors.New(
			"transparent TCP currently supports IPv4 only",
		)
	}

	dialer := net.Dialer{
		LocalAddr: &net.TCPAddr{
			IP:   ip4,
			Port: source.Port,
		},

		Control: func(
			network string,
			address string,
			raw syscall.RawConn,
		) error {
			var socketErr error

			err := raw.Control(func(fd uintptr) {
				if err := setIPTransparent(fd); err != nil {
					socketErr = err
					return
				}

				socketErr = unix.SetsockoptInt(
					int(fd),
					unix.SOL_SOCKET,
					unix.SO_REUSEADDR,
					1,
				)
			})

			if err != nil {
				return err
			}

			return socketErr
		},
	}

	conn, err := dialer.Dial("tcp4", target.String())
	if err != nil {
		return nil, err
	}

	tcp, ok := conn.(*net.TCPConn)
	if !ok {
		_ = conn.Close()
		return nil, errors.New("unexpected TCP connection type")
	}

	return tcp, nil
}

// UDP preserves the complete original source IP:port.
//
// We use a raw socket here so IP_TRANSPARENT is enabled before bind().
func transparentUDP(source *net.UDPAddr) (*net.UDPConn, error) {
	if source == nil {
		return nil, errors.New("nil UDP source address")
	}

	ip4 := source.IP.To4()
	if ip4 == nil {
		return nil, errors.New(
			"transparent UDP currently supports IPv4 only",
		)
	}

	fd, err := unix.Socket(
		unix.AF_INET,
		unix.SOCK_DGRAM|unix.SOCK_CLOEXEC,
		unix.IPPROTO_UDP,
	)
	if err != nil {
		return nil, fmt.Errorf("create UDP socket: %w", err)
	}

	ownedFD := true

	defer func() {
		if ownedFD {
			_ = unix.Close(fd)
		}
	}()

	if err := unix.SetsockoptInt(
		fd,
		unix.SOL_IP,
		unix.IP_TRANSPARENT,
		1,
	); err != nil {
		return nil, fmt.Errorf(
			"set UDP IP_TRANSPARENT: %w",
			err,
		)
	}

	var rawIP [4]byte
	copy(rawIP[:], ip4)

	sockaddr := &unix.SockaddrInet4{
		Port: source.Port,
		Addr: rawIP,
	}

	if err := unix.Bind(fd, sockaddr); err != nil {
		return nil, fmt.Errorf(
			"bind transparent UDP %s: %w",
			source,
			err,
		)
	}

	file := os.NewFile(
		uintptr(fd),
		fmt.Sprintf(
			"proxy2trans-udp-%s-%d",
			ip4.String(),
			source.Port,
		),
	)

	if file == nil {
		return nil, errors.New(
			"failed creating os.File for transparent UDP socket",
		)
	}

	// net.FilePacketConn duplicates the underlying descriptor.
	packetConn, err := net.FilePacketConn(file)

	_ = file.Close()

	if err != nil {
		return nil, fmt.Errorf(
			"convert UDP socket to net.PacketConn: %w",
			err,
		)
	}

	// The original fd was closed by file.Close().
	ownedFD = false

	udp, ok := packetConn.(*net.UDPConn)
	if !ok {
		_ = packetConn.Close()
		return nil, errors.New(
			"unexpected transparent UDP connection type",
		)
	}

	return udp, nil
}

// -----------------------------------------------------------------------------
// TCP proxy
// -----------------------------------------------------------------------------

func runTCPListener(
	ctx context.Context,
	cfg Listener,
) error {
	listener, err := net.Listen("tcp4", cfg.Listen)
	if err != nil {
		return fmt.Errorf(
			"TCP [%s] listen %s: %w",
			cfg.Name,
			cfg.Listen,
			err,
		)
	}

	defer listener.Close()

	target, err := net.ResolveTCPAddr("tcp4", cfg.Target)
	if err != nil {
		return fmt.Errorf(
			"TCP [%s] resolve target %s: %w",
			cfg.Name,
			cfg.Target,
			err,
		)
	}

	infof(
		"TCP [%s] listening=%s target=%s",
		cfg.Name,
		cfg.Listen,
		target,
	)

	go func() {
		<-ctx.Done()
		_ = listener.Close()
	}()

	for {
		conn, err := listener.Accept()
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}

			warnf(
				"TCP [%s] accept error: %v",
				cfg.Name,
				err,
			)

			continue
		}

		go handleTCP(cfg, conn, target)
	}
}

func handleTCP(
	cfg Listener,
	downstream net.Conn,
	target *net.TCPAddr,
) {
	defer downstream.Close()

	// Prevent a client from connecting and never completing the PPv2 header.
	_ = downstream.SetReadDeadline(
		time.Now().Add(10 * time.Second),
	)

	info, err := readTCPProxyHeader(downstream)
	if err != nil {
		warnf(
			"TCP [%s] PROXY v2 error peer=%s: %v",
			cfg.Name,
			downstream.RemoteAddr(),
			err,
		)
		return
	}

	// PPv2 was read successfully. Remove the temporary header deadline.
	_ = downstream.SetReadDeadline(time.Time{})

	sourceIP := info.Source.IP.To4()
	if sourceIP == nil {
		warnf(
			"TCP [%s] non-IPv4 source rejected: %s",
			cfg.Name,
			info.Source,
		)
		return
	}

	source := &net.TCPAddr{
		IP:   sourceIP,
		Port: info.Source.Port,
	}

	upstream, err := transparentDialTCP(
		source,
		target,
	)
	if err != nil {
		errorf(
			"TCP [%s] transparent connect client=%s target=%s failed: %v",
			cfg.Name,
			info.Source,
			target,
			err,
		)
		return
	}

	defer upstream.Close()

	debugf(
		"TCP [%s] OPEN client=%s original-dst=%s target=%s upstream-local=%s",
		cfg.Name,
		info.Source,
		info.Destination,
		target,
		upstream.LocalAddr(),
	)

	var wg sync.WaitGroup
	wg.Add(2)

	// Playit -> backend
	go func() {
		defer wg.Done()

		_, err := io.Copy(upstream, downstream)

		if err != nil &&
			!errors.Is(err, net.ErrClosed) {
			warnf(
				"TCP [%s] client->target copy error client=%s: %v",
				cfg.Name,
				info.Source,
				err,
			)
		}

		_ = upstream.CloseWrite()
	}()

	// Backend -> Playit
	go func() {
		defer wg.Done()

		_, err := io.Copy(downstream, upstream)

		if err != nil &&
			!errors.Is(err, net.ErrClosed) {
			warnf(
				"TCP [%s] target->client copy error client=%s: %v",
				cfg.Name,
				info.Source,
				err,
			)
		}

		if tcp, ok := downstream.(*net.TCPConn); ok {
			_ = tcp.CloseWrite()
		}
	}()

	wg.Wait()

	debugf(
		"TCP [%s] CLOSE client=%s target=%s",
		cfg.Name,
		info.Source,
		target,
	)
}

// -----------------------------------------------------------------------------
// UDP proxy
// -----------------------------------------------------------------------------

type UDPFlow struct {
	key string

	source     *net.UDPAddr
	playitPeer *net.UDPAddr
	target     *net.UDPAddr

	upstream *net.UDPConn

	mu       sync.Mutex
	lastSeen time.Time

	closeOnce sync.Once
}

func (f *UDPFlow) touch() {
	f.mu.Lock()
	f.lastSeen = time.Now()
	f.mu.Unlock()
}

func (f *UDPFlow) idleFor() time.Duration {
	f.mu.Lock()
	defer f.mu.Unlock()

	return time.Since(f.lastSeen)
}

type UDPProxy struct {
	cfg Listener

	listener *net.UDPConn
	target   *net.UDPAddr

	ctx context.Context

	mu sync.Mutex

	// flows is keyed by original client + Playit peer. peerFlows provides the
	// reverse association needed because Playit sends PROXY v2 once for a UDP
	// flow, followed by ordinary UDP payload datagrams from the same peer.
	flows     map[string]*UDPFlow
	peerFlows map[string]*UDPFlow
}

func udpFlowKey(
	source *net.UDPAddr,
	playitPeer *net.UDPAddr,
) string {
	return source.String() + "|" + playitPeer.String()
}

func (p *UDPProxy) getOrCreateFlow(
	source *net.UDPAddr,
	playitPeer *net.UDPAddr,
) (*UDPFlow, error) {
	key := udpFlowKey(source, playitPeer)

	p.mu.Lock()

	if existing := p.flows[key]; existing != nil {
		existing.touch()
		p.mu.Unlock()
		return existing, nil
	}

	upstream, err := transparentUDP(source)
	if err != nil {
		p.mu.Unlock()
		return nil, err
	}

	flow := &UDPFlow{
		key:        key,
		source:     cloneUDPAddr(source),
		playitPeer: cloneUDPAddr(playitPeer),
		target:     cloneUDPAddr(p.target),
		upstream:   upstream,
		lastSeen:   time.Now(),
	}

	p.flows[key] = flow
	p.peerFlows[playitPeer.String()] = flow

	p.mu.Unlock()

	debugf(
		"UDP [%s] OPEN client=%s playit=%s target=%s",
		p.cfg.Name,
		flow.source,
		flow.playitPeer,
		flow.target,
	)

	go p.readUDPReplies(flow)

	return flow, nil
}

func cloneUDPAddr(addr *net.UDPAddr) *net.UDPAddr {
	if addr == nil {
		return nil
	}

	ip := make(net.IP, len(addr.IP))
	copy(ip, addr.IP)

	return &net.UDPAddr{
		IP:   ip,
		Port: addr.Port,
		Zone: addr.Zone,
	}
}

func (p *UDPProxy) removeFlow(flow *UDPFlow) {
	if flow == nil {
		return
	}

	flow.closeOnce.Do(func() {
		p.mu.Lock()

		if current := p.flows[flow.key]; current == flow {
			delete(p.flows, flow.key)
		}

		peerKey := flow.playitPeer.String()
		if current := p.peerFlows[peerKey]; current == flow {
			delete(p.peerFlows, peerKey)
		}

		p.mu.Unlock()

		_ = flow.upstream.Close()

		debugf(
			"UDP [%s] CLOSE client=%s playit=%s target=%s",
			p.cfg.Name,
			flow.source,
			flow.playitPeer,
			flow.target,
		)
	})
}

func (p *UDPProxy) closeAllFlows() {
	p.mu.Lock()

	flows := make([]*UDPFlow, 0, len(p.flows))

	for _, flow := range p.flows {
		flows = append(flows, flow)
	}

	p.mu.Unlock()

	for _, flow := range flows {
		p.removeFlow(flow)
	}
}

func (p *UDPProxy) readUDPReplies(flow *UDPFlow) {
	buffer := make([]byte, maxUDPDatagram)

	for {
		select {
		case <-p.ctx.Done():
			p.removeFlow(flow)
			return

		default:
		}

		_ = flow.upstream.SetReadDeadline(
			time.Now().Add(p.cfg.IdleTimeout),
		)

		n, from, err := flow.upstream.ReadFromUDP(buffer)
		if err != nil {
			if p.ctx.Err() != nil {
				p.removeFlow(flow)
				return
			}

			if errors.Is(err, net.ErrClosed) {
				return
			}

			var netErr net.Error

			if errors.As(err, &netErr) && netErr.Timeout() {
				if flow.idleFor() >= p.cfg.IdleTimeout {
					p.removeFlow(flow)
					return
				}

				continue
			}

			warnf(
				"UDP [%s] target read error client=%s: %v",
				p.cfg.Name,
				flow.source,
				err,
			)

			p.removeFlow(flow)
			return
		}

		flow.touch()

		_, err = p.listener.WriteToUDP(
			buffer[:n],
			flow.playitPeer,
		)
		if err != nil {
			if p.ctx.Err() != nil {
				p.removeFlow(flow)
				return
			}

			warnf(
				"UDP [%s] reply to Playit failed client=%s playit=%s: %v",
				p.cfg.Name,
				flow.source,
				flow.playitPeer,
				err,
			)

			p.removeFlow(flow)
			return
		}

		debugf(
			"UDP [%s] RX target=%s client=%s playit=%s bytes=%d",
			p.cfg.Name,
			from,
			flow.source,
			flow.playitPeer,
			n,
		)
	}
}

func runUDPListener(
	ctx context.Context,
	cfg Listener,
) error {
	listenAddr, err := net.ResolveUDPAddr(
		"udp4",
		cfg.Listen,
	)
	if err != nil {
		return fmt.Errorf(
			"UDP [%s] resolve listen %s: %w",
			cfg.Name,
			cfg.Listen,
			err,
		)
	}

	listener, err := net.ListenUDP(
		"udp4",
		listenAddr,
	)
	if err != nil {
		return fmt.Errorf(
			"UDP [%s] listen %s: %w",
			cfg.Name,
			cfg.Listen,
			err,
		)
	}

	defer listener.Close()

	target, err := net.ResolveUDPAddr(
		"udp4",
		cfg.Target,
	)
	if err != nil {
		return fmt.Errorf(
			"UDP [%s] resolve target %s: %w",
			cfg.Name,
			cfg.Target,
			err,
		)
	}

	proxy := &UDPProxy{
		cfg:       cfg,
		listener:  listener,
		target:    target,
		ctx:       ctx,
		flows:     make(map[string]*UDPFlow),
		peerFlows: make(map[string]*UDPFlow),
	}

	infof(
		"UDP [%s] listening=%s target=%s idle=%s",
		cfg.Name,
		cfg.Listen,
		target,
		cfg.IdleTimeout,
	)

	go func() {
		<-ctx.Done()

		_ = listener.Close()
		proxy.closeAllFlows()
	}()

	buffer := make([]byte, maxUDPDatagram)

	for {
		n, peer, err := listener.ReadFromUDP(buffer)
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}

			warnf(
				"UDP [%s] read error: %v",
				cfg.Name,
				err,
			)

			continue
		}

		packet := buffer[:n]

		// Playit sends one PROXY v2 datagram to establish the UDP flow and then
		// sends ordinary payload datagrams from the same UDP peer. First try to
		// parse a PPv2 header; if there is no signature, use the established
		// peer->flow association instead of treating the payload as malformed.
		if len(packet) >= len(proxyV2Signature) &&
			bytes.Equal(packet[:len(proxyV2Signature)], proxyV2Signature) {
			info, err := parseProxyV2(packet)
			if err != nil {
				warnf(
					"UDP [%s] invalid PROXY v2 peer=%s bytes=%d: %v",
					cfg.Name,
					peer,
					n,
					err,
				)
				continue
			}

			// PROXY v2 transport: 0x2 = DGRAM.
			if info.Transport != 0x2 {
				warnf(
					"UDP [%s] invalid transport client=%s: got=0x%x expected=DGRAM",
					cfg.Name,
					info.Source,
					info.Transport,
				)
				continue
			}

			if info.HeaderLen > len(packet) {
				warnf(
					"UDP [%s] invalid payload offset peer=%s header=%d packet=%d",
					cfg.Name,
					peer,
					info.HeaderLen,
					len(packet),
				)
				continue
			}

			flow, err := proxy.getOrCreateFlow(info.Source, peer)
			if err != nil {
				errorf(
					"UDP [%s] create transparent flow client=%s target=%s failed: %v",
					cfg.Name,
					info.Source,
					target,
					err,
				)
				continue
			}

			flow.touch()
			payload := packet[info.HeaderLen:]

			// A header-only PPv2 datagram establishes the association but carries
			// no application payload, so do not emit a zero-length backend packet.
			if len(payload) == 0 {
				debugf(
					"UDP [%s] ASSOC client=%s original-dst=%s playit=%s target=%s",
					cfg.Name,
					info.Source,
					info.Destination,
					peer,
					target,
				)
				continue
			}

			_, err = flow.upstream.WriteToUDP(payload, target)
			if err != nil {
				warnf(
					"UDP [%s] send client=%s target=%s failed: %v",
					cfg.Name,
					info.Source,
					target,
					err,
				)
				proxy.removeFlow(flow)
				continue
			}

			debugf(
				"UDP [%s] TX client=%s original-dst=%s target=%s bytes=%d",
				cfg.Name,
				info.Source,
				info.Destination,
				target,
				len(payload),
			)
			continue
		}

		// No PPv2 signature: this is an application datagram belonging to an
		// already-established Playit UDP flow. Associate it by Playit's source
		// IP:port, which remains stable for that flow.
		proxy.mu.Lock()
		flow := proxy.peerFlows[peer.String()]
		proxy.mu.Unlock()

		if flow == nil {
			warnf(
				"UDP [%s] payload without PROXY v2 association peer=%s bytes=%d",
				cfg.Name,
				peer,
				n,
			)
			continue
		}

		flow.touch()

		_, err = flow.upstream.WriteToUDP(packet, target)
		if err != nil {
			warnf(
				"UDP [%s] send client=%s target=%s failed: %v",
				cfg.Name,
				flow.source,
				target,
				err,
			)
			proxy.removeFlow(flow)
			continue
		}

		debugf(
			"UDP [%s] TX client=%s target=%s bytes=%d",
			cfg.Name,
			flow.source,
			target,
			len(packet),
		)
	}
}

// -----------------------------------------------------------------------------
// Main
// -----------------------------------------------------------------------------

func main() {
	log.SetFlags(
		log.LstdFlags |
			log.Lmicroseconds |
			log.LUTC,
	)

	configPath := "/etc/proxy2trans/config.yml"

	if len(os.Args) > 1 {
		configPath = os.Args[1]
	}

	cfg, err := loadConfig(configPath)
	if err != nil {
		log.Fatalf("[ERROR] configuration error: %v", err)
	}

	level, _ := parseLogLevel(cfg.LogLevel)
	setLogLevel(level)

	infof(
		"proxy2trans starting config=%s listeners=%d log_level=%s",
		configPath,
		len(cfg.Listeners),
		cfg.LogLevel,
	)

	ctx, stop := signal.NotifyContext(
		context.Background(),
		syscall.SIGINT,
		syscall.SIGTERM,
	)
	defer stop()

	errCh := make(chan error, len(cfg.Listeners))

	var wg sync.WaitGroup

	for _, listenerCfg := range cfg.Listeners {
		listenerCfg := listenerCfg

		wg.Add(1)

		go func() {
			defer wg.Done()

			var err error

			switch listenerCfg.Protocol {
			case "tcp":
				err = runTCPListener(
					ctx,
					listenerCfg,
				)

			case "udp":
				err = runUDPListener(
					ctx,
					listenerCfg,
				)

			default:
				err = fmt.Errorf(
					"listener %q: unsupported protocol %q",
					listenerCfg.Name,
					listenerCfg.Protocol,
				)
			}

			if err != nil {
				select {
				case errCh <- err:
				default:
				}
			}
		}()
	}

	select {
	case <-ctx.Done():
		infof("shutdown requested")

	case err := <-errCh:
		if err != nil {
			errorf("listener failure: %v", err)
			stop()
		}
	}

	wg.Wait()

	infof("proxy2trans stopped")
}
