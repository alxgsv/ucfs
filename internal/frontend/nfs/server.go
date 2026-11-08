package nfs

import (
	"errors"
	"log/slog"
	"net"

	gonfs "github.com/willscott/go-nfs"

	"github.com/alxgsv/ucfs/internal/vfs"
)

// ConcurrentHandlers lets requests on the client's single connection run in
// parallel; with 1 a slow read stalls every process on the mount (Spike B).
const ConcurrentHandlers = 128

type Server struct {
	l    net.Listener
	done chan error
}

// Serve exports v on a random loopback port. It never listens beyond
// 127.0.0.1, because the export has no authentication.
func Serve(v *vfs.FS) (*Server, error) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, err
	}
	s := &Server{l: l, done: make(chan error, 1)}
	srv := &gonfs.Server{Handler: newHandler(v), ConcurrentHandlers: ConcurrentHandlers}
	go func() { s.done <- srv.Serve(l) }()
	return s, nil
}

func (s *Server) Port() int { return s.l.Addr().(*net.TCPAddr).Port }

func (s *Server) Close() error {
	err := s.l.Close()
	if serr := <-s.done; serr != nil && !errors.Is(serr, net.ErrClosed) {
		slog.Debug("nfs server stopped", "err", serr)
	}
	return err
}
