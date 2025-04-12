package main

/*
build on raspberry pi
GOHOSTARCH=arm GOARCH=arm go build *.go
*/

import (
	"bytes"
	"context"
	"flag"
	"fmt"
	"net"
	"net/netip"
	"os"
	"os/signal"
	"strings"

	"go.uber.org/zap"
)

type cmdParams struct {
	listenAddr netip.AddrPort
	destAddr   netip.AddrPort
	outFiles   []string
}

type filesList []string

func (fl *filesList) String() string {
	return strings.Join(*fl, ",")
}
func (fl *filesList) Set(value string) error {
	*fl = append(*fl, value)
	return nil
}

func parseCMDParams(args []string) (cmdParams, error) {
	dest := ""
	listen := ""
	outFiles := filesList{}

	fs := flag.NewFlagSet("rtl_fanout", flag.ExitOnError)
	fs.StringVar(&dest, "d", "", "destination ip:port, 1.2.3.4:123")
	fs.StringVar(&listen, "l", "", "listen on ip:port, 0.0.0.0:123")
	fs.Var(&outFiles, "f", "/path/to/file")

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
		outFiles:   outFiles,
	}, nil
}

type dongleInfo []byte

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

	fanoutSink := newFanoutSink()

	for _, filePath := range p.params.outFiles {
		go func() {
			for {
				sink, err := newFileSink(p.logger, filePath)
				if err != nil {
					p.logger.Error("failed to open file", zap.String("dest file path", filePath), zap.Error(err))
					continue
				}
				fanoutSink.AddSink(sink)
				_ = sink.Run(ctx)
				fanoutSink.RemoveSink(sink)
			}
		}()
	}

	go func() {
		<-ctx.Done()
		p.logger.Info("context cancel")
		listener.Close()
		destConn.Close()
	}()

	dongleInfo, err := p.readDongleInfo(destConn)
	if err != nil {
		return err
	}
	p.dongleInfo = dongleInfo
	p.logger.Info("got dongle info", zap.String("address", listenTcpAddr.String()))

	go func() {
		for {
			conn, err := listener.AcceptTCP()
			if err != nil {
				p.logger.Error("accept call failed", zap.Error(err))
				continue
			}
			s, err := newTcpSocketSink(p.logger, conn)
			if err != nil {
				p.logger.Error("faild to create a sync", zap.Error(err))
				continue
			}
			go func() {
				s.Send(p.dongleInfo)
				fanoutSink.AddSink(s)
				_ = s.Run(ctx)
				fanoutSink.RemoveSink(s)
			}()
		}
	}()
	for {
		const bufSize = 1000
		buf := make([]byte, bufSize)
		read, err := destConn.Read(buf)
		if err != nil {
			fmt.Println("failed to read from connection: %w", err)
			// FIXME  exit?
			destConn.Close()
			return nil
		}
		fanoutSink.Send(buf[:read])
	}
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
