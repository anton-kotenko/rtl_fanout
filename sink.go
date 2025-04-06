package main

import (
	"context"
	"fmt"
	"net"
	"os"
	"sync"
	"sync/atomic"

	"go.uber.org/zap"
)

type Sink interface {
	Run(ctx context.Context) error
	Send(chunk []byte)
}

type fanoutSink struct {
	children sync.Map
}

func newFanoutSink() *fanoutSink {
	return &fanoutSink{
		children: sync.Map{},
	}
}

func (f *fanoutSink) Run(_ context.Context) error {
	return nil
}

func (f *fanoutSink) Send(chunk []byte) {
	f.children.Range(func(k, v any) bool {
		castedToSink, ok := v.(Sink)
		if ok {
			castedToSink.Send(chunk)
		}
		return true
	})
}

func (f *fanoutSink) AddSink(s Sink) {
	f.children.Store(s, s)
}

func (f *fanoutSink) RemoveSink(s Sink) {
	f.children.Delete(s)
}

type tcpSocketSink struct {
	conn    *net.TCPConn
	logger  *zap.Logger
	buffers chan []byte
}

func newTcpSocketSink(logger *zap.Logger, conn *net.TCPConn) (Sink, error) {
	const bufSize = 10000
	remoteAddr := conn.RemoteAddr()
	return &tcpSocketSink{
		logger:  logger.With(zap.String("remote_addr", remoteAddr.String())),
		conn:    conn,
		buffers: make(chan []byte, bufSize),
	}, nil
}

func (s *tcpSocketSink) Run(ctx context.Context) error {
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

func (s *tcpSocketSink) Send(chunk []byte) {
	// FIXME if already closed -- ignore chunk
	select {
	case s.buffers <- chunk:
	default:
		s.logger.Warn("sink overflow")
	}
}

type fileSink struct {
	logger       *zap.Logger
	buffers      chan []byte
	filePath     string
	seenOverflow atomic.Bool
}

func newFileSink(logger *zap.Logger, path string) (Sink, error) {
	const bufSize = 10000
	return &fileSink{
		logger:       logger.With(zap.String("dest_path", path)),
		buffers:      make(chan []byte, bufSize),
		filePath:     path,
		seenOverflow: atomic.Bool{},
	}, nil
}

func (s *fileSink) Run(ctx context.Context) error {
	s.logger.Info("starting sink")
	file, err := os.OpenFile(s.filePath, os.O_WRONLY, 0777)
	if err != nil {
		return fmt.Errorf("failed to open dest file %q: %w", s.filePath, err)
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	go func() {
		<-ctx.Done()
		file.Close()
	}()
	defer file.Close() // FIXME might already be closed

	for {
		chunk := <-s.buffers
		_, err := file.Write(chunk)
		if err != nil {
			s.logger.Error("failed to write bytes", zap.Error(err))
			break
		}
	}

	err = file.Close()
	s.logger.Info("closing sync", zap.Error(err))
	return err
}

func (s *fileSink) Send(chunk []byte) {
	select {
	case s.buffers <- chunk:
		s.seenOverflow.Store(false)
	default:
		if !s.seenOverflow.Load() {
			s.seenOverflow.Store(true)
			s.logger.Warn("sink overflow")
		}
	}
}
