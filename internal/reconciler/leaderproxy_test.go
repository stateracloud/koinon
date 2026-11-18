// SPDX-License-Identifier: Apache-2.0

package reconciler_test

import (
	"context"
	"net"
	"strconv"
	"sync"
	"sync/atomic"

	"github.com/jackc/pgx/v5/pgxpool"
	. "github.com/onsi/gomega"
)

// silentProxy forwards TCP to a Postgres until freeze is called, then reads and drops every byte
// in both directions and keeps the sockets open: no RST, no FIN, and its kernel still answers TCP
// keepalive probes. That is a vanished peer as the leader sees it (hard node loss, partition,
// a proxy in between), the case issue 635 is about.
type silentProxy struct {
	ln     net.Listener
	target string
	frozen atomic.Bool
	mu     sync.Mutex
	conns  []net.Conn
}

func newSilentProxy(target string) *silentProxy {
	ln, err := (&net.ListenConfig{}).Listen(context.Background(), "tcp", "127.0.0.1:0")
	Expect(err).NotTo(HaveOccurred())
	p := &silentProxy{ln: ln, target: target}
	go p.accept()
	return p
}

func (p *silentProxy) addr() (host string, port uint16) {
	h, ps, err := net.SplitHostPort(p.ln.Addr().String())
	Expect(err).NotTo(HaveOccurred())
	n, err := strconv.Atoi(ps)
	Expect(err).NotTo(HaveOccurred())
	return h, uint16(n)
}

func (p *silentProxy) freeze() { p.frozen.Store(true) }

func (p *silentProxy) close() {
	_ = p.ln.Close()
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, c := range p.conns {
		_ = c.Close()
	}
}

func (p *silentProxy) accept() {
	for {
		c, err := p.ln.Accept()
		if err != nil {
			return
		}
		up, err := (&net.Dialer{}).DialContext(context.Background(), "tcp", p.target)
		if err != nil {
			_ = c.Close()
			continue
		}
		p.mu.Lock()
		p.conns = append(p.conns, c, up)
		p.mu.Unlock()
		go p.pipe(up, c)
		go p.pipe(c, up)
	}
}

func (p *silentProxy) pipe(dst, src net.Conn) {
	buf := make([]byte, 32<<10)
	for {
		n, err := src.Read(buf)
		if n > 0 && !p.frozen.Load() {
			if _, werr := dst.Write(buf[:n]); werr != nil {
				return
			}
		}
		if err != nil {
			return
		}
	}
}

// poolVia returns a pool on the same database as base that connects through the proxy.
func poolVia(ctx context.Context, base *pgxpool.Pool, p *silentProxy) *pgxpool.Pool {
	cfg, err := pgxpool.ParseConfig(base.Config().ConnString())
	Expect(err).NotTo(HaveOccurred())
	cfg.ConnConfig.Host, cfg.ConnConfig.Port = p.addr()
	cfg.ConnConfig.Fallbacks = nil
	out, err := pgxpool.NewWithConfig(ctx, cfg)
	Expect(err).NotTo(HaveOccurred())
	return out
}
