package http

import (
	"net"
	"sync/atomic"
)

type RoundRobinServer struct {
	Servers     []*Server
	EventLogger EventLogger
	idx         uint64
}

func (s *RoundRobinServer) Serve(listener net.Listener) error {
	for {
		conn, err := listener.Accept()
		if err != nil {
			return err
		}
		go s.dispatch(conn)
	}
}

func (s *RoundRobinServer) dispatch(conn net.Conn) {
	h := s.next()
	h.dispatch(conn)
}

// next returns the next item in the slice. When the end of the slice is reached, it starts again from the beginning.
func (s *RoundRobinServer) next() *Server {
	idx := atomic.AddUint64(&s.idx, 1) - 1
	return s.Servers[idx%uint64(len(s.Servers))]
}
