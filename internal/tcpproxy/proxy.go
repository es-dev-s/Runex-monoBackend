package tcpproxy

import (
	"io"
	"net"
	"sync"
	"time"
)

// Server forwards TCP to a target resolved per connection so a container
// restart that changes the Docker IP does not require a new listen port.
type Server struct {
	ln      net.Listener
	resolve func() (string, error)
	done    chan struct{}
	wg      sync.WaitGroup
}

func Listen(addr string, resolve func() (string, error)) (*Server, error) {
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return nil, err
	}
	s := &Server{ln: ln, resolve: resolve, done: make(chan struct{})}
	s.wg.Add(1)
	go s.serve()
	return s, nil
}

func (s *Server) Addr() string {
	if s == nil || s.ln == nil {
		return ""
	}
	return s.ln.Addr().String()
}

func (s *Server) Close() error {
	if s == nil {
		return nil
	}
	select {
	case <-s.done:
	default:
		close(s.done)
	}
	if s.ln != nil {
		_ = s.ln.Close()
	}
	s.wg.Wait()
	return nil
}

func (s *Server) serve() {
	defer s.wg.Done()
	for {
		conn, err := s.ln.Accept()
		if err != nil {
			select {
			case <-s.done:
				return
			default:
				return
			}
		}
		s.wg.Add(1)
		go s.handle(conn)
	}
}

func (s *Server) handle(src net.Conn) {
	defer s.wg.Done()
	defer src.Close()
	target, err := s.resolve()
	if err != nil || target == "" {
		return
	}
	dst, err := net.DialTimeout("tcp", target, 3*time.Second)
	if err != nil {
		return
	}
	defer dst.Close()
	_ = src.SetDeadline(time.Now().Add(30 * time.Minute))
	_ = dst.SetDeadline(time.Now().Add(30 * time.Minute))
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		_, _ = io.Copy(dst, src)
		_ = dst.SetDeadline(time.Now())
	}()
	go func() {
		defer wg.Done()
		_, _ = io.Copy(src, dst)
		_ = src.SetDeadline(time.Now())
	}()
	wg.Wait()
}
