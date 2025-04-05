package main

import (
	"bytes"
	"context"
	"flag"
	"fmt"
	"net"
	"net/netip"
	"os"
	"os/signal"
	"sync"

	"golang.org/x/exp/slices"

	"go.uber.org/zap"
)

type cmdParams struct {
	listenAddr netip.AddrPort
	destAddr   netip.AddrPort
}

func parseCMDParams(args []string) (cmdParams, error) {
	dest := ""
	listen := ""

	fs := flag.NewFlagSet("rtl_fanout", flag.ExitOnError)
	fs.StringVar(&dest, "d", "", "destination ip:port, 1.2.3.4:123")
	fs.StringVar(&listen, "l", "", "listen on ip:port, 0.0.0.0:123")

	decorateError := func(err error) error {
		if err == nil {
			return err
		}

		defaults := bytes.NewBufferString("")
		fs.SetOutput(defaults)
		fs.PrintDefaults()
		return fmt.Errorf("%w\n\nUsage:\n%s", err, defaults)
	}

	err := fs.Parse(args[1:])
	if err != nil {
		return cmdParams{}, decorateError(err)
	}

	destAddr, err := netip.ParseAddrPort(dest)
	if err != nil {
		return cmdParams{}, decorateError(fmt.Errorf("failed to parse destination address %q as net address", dest))
	}

	listenAddr, err := netip.ParseAddrPort(listen)
	if err != nil {
		return cmdParams{}, decorateError(fmt.Errorf("failed to parse listen address %q as net address", listen))
	}

	return cmdParams{
		listenAddr: listenAddr,
		destAddr:   destAddr,
	}, nil
}

type dongleInfo []byte

type sink struct {
	conn    *net.TCPConn
	logger  *zap.Logger
	buffers chan []byte
}

func newSink(logger *zap.Logger, conn *net.TCPConn) (*sink, error) {
	const bufSize = 10000
	remoteAddr := conn.RemoteAddr()
	s := sink{
		logger:  logger.With(zap.String("remote_addr", remoteAddr.String())),
		conn:    conn,
		buffers: make(chan []byte, bufSize),
	}
	return &s, nil
}

func (s *sink) run(ctx context.Context) error {
	s.logger.Info("starting sink")
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	go func() {
		<-ctx.Done()
		s.conn.Close()
	}()
	defer s.conn.Close() // FIXME might already be closed

	wg := sync.WaitGroup{}
	wg.Add(2)
	go func() {
		defer wg.Done()
		const bufSize = 100
		buf := make([]byte, bufSize)
		for {
			_, err := s.conn.Read(buf)
			if err != nil {
				s.logger.Warn("client read failed, likely socket close", zap.Error(err))
				break
			}
			s.logger.Warn("unexpected chunk from client, ignore")
		}
	}()
	go func() {
		wg.Done()
		for {
			chunk := <-s.buffers
			_, err := s.conn.Write(chunk)
			if err != nil {
				s.logger.Error("failed to write bytes", zap.Error(err))
				break
			}
		}
	}()

	wg.Wait()
	err := s.conn.Close()
	s.logger.Info("closing sync", zap.Error(err))
	return err
}

func (s *sink) send(chunk []byte) {
	// FIXME if already closed -- ignore chunk
	select {
	case s.buffers <- chunk:
	default:
		s.logger.Warn("sink overflow")
	}
}

type fanoutProxy struct {
	dongleInfo []byte
	params     cmdParams
	logger     *zap.Logger
}

func newFanoutProxy(params cmdParams, logger *zap.Logger) (*fanoutProxy, error) {
	return &fanoutProxy{
		params: params,
		logger: logger,
	}, nil
}

func (p *fanoutProxy) Run(ctx context.Context) error {
	listenTcpAddr := net.TCPAddrFromAddrPort(p.params.listenAddr)
	listener, err := net.ListenTCP(listenTcpAddr.Network(), listenTcpAddr)
	if err != nil {
		return fmt.Errorf("failed to open listening socket on %q: %w", listenTcpAddr.String(), err)
	}
	p.logger.Info("start listening", zap.String("address", listenTcpAddr.String()))

	destTcpAddr := net.TCPAddrFromAddrPort(p.params.destAddr)
	destConn, err := net.DialTCP(destTcpAddr.Network(), nil, destTcpAddr)
	if err != nil {
		return fmt.Errorf("failed to connect to %q: %w", destTcpAddr.String(), err)
	}
	p.logger.Info("connected to source", zap.String("address", listenTcpAddr.String()))

	go func() {
		<-ctx.Done()
		listener.Close()
		destConn.Close()
	}()

	dongleInfo, err := p.readDongleInfo(destConn)
	if err != nil {
		return err
	}
	p.dongleInfo = dongleInfo
	p.logger.Info("got dongle info", zap.String("address", listenTcpAddr.String()))

	var mu sync.RWMutex
	destinations := []*sink{}

	go func() {
		for {
			conn, err := listener.AcceptTCP()
			if err != nil {
				p.logger.Error("accept call failed", zap.Error(err))
				continue
			}
			s, err := newSink(p.logger, conn)
			if err != nil {
				p.logger.Error("faild to create a sync", zap.Error(err))
				continue
			}
			go func() {
				s.send(p.dongleInfo)
				mu.Lock()
				destinations = append(destinations, s)
				mu.Unlock()
				_ = s.run(ctx)
				mu.Lock()
				defer mu.Unlock()
				destinations = slices.DeleteFunc(destinations, func(elem *sink) bool {
					return elem == s
				})
			}()
		}
	}()
	for {
		const bufSize = 1000
		buf := make([]byte, bufSize)
		_, err := destConn.Read(buf)
		if err != nil {
			fmt.Println("failed to read from connection: %w", err)
			// FIXME  exit?
			destConn.Close()
			return nil
		}

		mu.RLock()
		dests := make([]*sink, len(destinations))
		copy(dests, destinations)
		mu.RUnlock()
		for _, dest := range dests {
			dest.send(buf)
		}
	}
}

func (p *fanoutProxy) handleInboundConnection(ctx context.Context, conn *net.TCPConn, data chan []byte) {
	go func() {
		<-ctx.Done()
		// FIXME double close?
		conn.Close()
	}()
	go func() {
		const bufSize = 100
		buf := make([]byte, bufSize)
		for {
			_, err := conn.Read(buf)
			if err != nil {
				fmt.Println("failed to read from connection: %w", err)
				conn.Close()
				return
			} else {
				fmt.Println("unexpected chunk of data, ignore!")
			}
		}
	}()
	go func() {
		for {
			chunk, ok := <-data
			if !ok {
				conn.Close()
			}
			_, err := conn.Write(chunk)
			if err != nil {
				fmt.Println("failed to write bytes", err)
			}
		}
	}()
}

func (p *fanoutProxy) readDongleInfo(conn *net.TCPConn) (dongleInfo, error) {
	const dongleInfoSize = 8
	leftBytes := dongleInfoSize
	var info []byte
	for leftBytes > 0 {
		buf := make([]byte, leftBytes)
		read, err := conn.Read(buf)
		if err != nil {
			return nil, fmt.Errorf("failed to read dongle info: %w", err)
		}
		info = append(info, buf...)
		leftBytes -= read
	}
	return info, nil
}

func main() {
	params, err := parseCMDParams(os.Args)
	if err != nil {
		fmt.Fprint(os.Stderr, err.Error())
		os.Exit(1)
	}

	ctx := context.Background()
	ctx, cancel := signal.NotifyContext(ctx, os.Interrupt)
	defer cancel()

	logger, err := zap.NewProduction()
	if err != nil {
		fmt.Fprint(os.Stderr, err.Error())
		os.Exit(1)
	}
	defer logger.Sync()

	proxy, err := newFanoutProxy(params, logger)
	if err != nil {
		logger.Error("failed to create a fanout proxy", zap.Error(err))
		os.Exit(1)
	}

	err = proxy.Run(ctx)
	if err != nil {
		logger.Error("proxy failed", zap.Error(err))
		os.Exit(1)
	}
}
