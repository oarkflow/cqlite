package mysql

import (
	"context"
	"errors"
	"net"

	db "github.com/sujit/sqlite"
)

type Server struct {
	DB   *db.DB
	Addr string
}

func (s *Server) ListenAndServe(ctx context.Context) error {
	if s.DB == nil {
		return errors.New("database is required")
	}
	addr := s.Addr
	if addr == "" {
		addr = "127.0.0.1:3306"
	}
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return err
	}
	defer ln.Close()
	go func() {
		<-ctx.Done()
		_ = ln.Close()
	}()
	return errors.New("mysql wire protocol handshake is scaffolded but not implemented yet")
}
