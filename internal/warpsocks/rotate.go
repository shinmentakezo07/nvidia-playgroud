// Package warpsocks exposes several SOCKS5 exits as a single SOCKS5 proxy and
// assigns a different exit to every new TCP connection.
//
// The captcha pipeline in cmd/serve pins one upstream connection per solve and
// re-dials after a failed extraction, so handing each new connection to the
// next exit is what makes that rotate: a solve that hits an exhausted hCaptcha
// per-IP budget is retried from a different egress address. Exits are plain
// SOCKS5 endpoints (a WARP client such as usque, or any other rotating pool);
// this package only does the selection and the inbound handshake.
package warpsocks

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/url"
	"sync"
	"sync/atomic"
	"time"

	xproxy "golang.org/x/net/proxy"
)

// Pool picks a SOCKS5 exit for each new connection.
//
// Round-robin keeps consecutive solves on different addresses; an exit that
// fails to dial is skipped for the rest of the round and resurrected on the
// next Pick, because a WARP tunnel that dropped usually reconnects within
// seconds and permanently retiring an exit would shrink the pool to one.
type Pool struct {
	exits  []string
	next   atomic.Uint64
	failed sync.Map // exit -> time of last failure
	dial   func(ctx context.Context, exit, addr string) (net.Conn, error)
}

// NewPool builds a pool from "host:port" SOCKS5 exits.
func NewPool(exits []string) (*Pool, error) {
	if len(exits) == 0 {
		return nil, errors.New("warpsocks: no exits")
	}
	for _, e := range exits {
		if _, _, err := net.SplitHostPort(e); err != nil {
			return nil, fmt.Errorf("warpsocks: bad exit %q: %w", e, err)
		}
	}
	p := &Pool{exits: append([]string(nil), exits...)}
	p.dial = func(ctx context.Context, exit, addr string) (net.Conn, error) {
		return dialSocks5(ctx, exit, addr)
	}
	return p, nil
}

// Exits returns the configured exit list.
func (p *Pool) Exits() []string { return append([]string(nil), p.exits...) }

// order returns the exits to try for one connection, starting at the
// round-robin cursor. Recently failed exits go last rather than being dropped,
// so a small pool still has something to fall back to.
func (p *Pool) order() []string {
	n := len(p.exits)
	start := int(p.next.Add(1)-1) % n
	out := make([]string, 0, n)
	var failed []string
	now := time.Now()
	for i := 0; i < n; i++ {
		e := p.exits[(start+i)%n]
		if v, ok := p.failed.Load(e); ok && now.Sub(v.(time.Time)) < 30*time.Second {
			failed = append(failed, e)
			continue
		}
		out = append(out, e)
	}
	return append(out, failed...)
}

// Dial connects to addr through one exit, trying the next exit when a tunnel
// refuses the connection. It returns the exit that served the connection.
func (p *Pool) Dial(ctx context.Context, addr string) (net.Conn, string, error) {
	var firstErr error
	for _, exit := range p.order() {
		c, err := p.dial(ctx, exit, addr)
		if err == nil {
			p.failed.Delete(exit)
			return c, exit, nil
		}
		p.failed.Store(exit, time.Now())
		if firstErr == nil {
			firstErr = fmt.Errorf("%s: %w", exit, err)
		}
	}
	return nil, "", firstErr
}

// Server is a SOCKS5 listener rotating over a Pool. Only CONNECT is
// implemented: the captcha solver and the catalog scraper need TCP tunnels
// only, and refusing the rest keeps the surface small.
type Server struct {
	Addr   string
	Pool   *Pool
	Logger *log.Logger // nil silences per-connection logs
}

// ListenAndServe accepts connections until ctx is cancelled.
func (s *Server) ListenAndServe(ctx context.Context) error {
	ln, err := net.Listen("tcp", s.Addr)
	if err != nil {
		return err
	}
	defer ln.Close()
	go func() {
		<-ctx.Done()
		ln.Close()
	}()
	s.logf("warpsocks: listening on %s over %d exits", ln.Addr(), len(s.Pool.exits))
	for {
		c, err := ln.Accept()
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			return err
		}
		go s.handle(ctx, c)
	}
}

func (s *Server) logf(format string, args ...any) {
	if s.Logger != nil {
		s.Logger.Printf(format, args...)
	}
}

func (s *Server) handle(ctx context.Context, c net.Conn) {
	defer c.Close()
	// The browser path of the PoW solver keeps one connection alive for a
	// whole solve; cap idle gaps so a stalled client cannot pin a tunnel.
	_ = c.SetDeadline(time.Now().Add(2 * time.Minute))
	dest, err := socks5ServerHandshake(c)
	if err != nil {
		s.logf("warpsocks: handshake from %s failed: %v", c.RemoteAddr(), err)
		return
	}
	up, exit, err := s.Pool.Dial(ctx, dest)
	if err != nil {
		s.logf("warpsocks: %s via pool failed: %v", dest, err)
		// 0x01 general failure, 0x05 connection refused: report the latter
		// because every exit refused, which is what the caller retries on.
		writeSocks5Reply(c, 0x05, nil)
		return
	}
	defer up.Close()
	if err := writeSocks5Reply(c, 0x00, up.LocalAddr()); err != nil {
		s.logf("warpsocks: reply to %s failed: %v", c.RemoteAddr(), err)
		return
	}
	s.logf("warpsocks: %s -> %s via %s", dest, up.RemoteAddr(), exit)
	_ = c.SetDeadline(time.Time{})
	splice(c, up)
}

// splice copies both directions until either side closes.
func splice(a, b net.Conn) {
	done := make(chan struct{}, 2)
	go func() { _, _ = io.Copy(a, b); done <- struct{}{} }()
	go func() { _, _ = io.Copy(b, a); done <- struct{}{} }()
	<-done
}

// socks5ServerHandshake performs the inbound no-auth greeting and returns the
// requested destination in "host:port" form.
func socks5ServerHandshake(c net.Conn) (string, error) {
	var hdr [2]byte
	if _, err := io.ReadFull(c, hdr[:]); err != nil {
		return "", err
	}
	if hdr[0] != 0x05 {
		return "", fmt.Errorf("unsupported version %d", hdr[0])
	}
	methods := make([]byte, hdr[1])
	if _, err := io.ReadFull(c, methods); err != nil {
		return "", err
	}
	ok := false
	for _, m := range methods {
		if m == 0x00 {
			ok = true
		}
	}
	if !ok {
		_, _ = c.Write([]byte{0x05, 0xff})
		return "", errors.New("no acceptable auth method")
	}
	if _, err := c.Write([]byte{0x05, 0x00}); err != nil {
		return "", err
	}
	var req [4]byte
	if _, err := io.ReadFull(c, req[:]); err != nil {
		return "", err
	}
	if req[1] != 0x01 { // CONNECT only
		writeSocks5Reply(c, 0x07, nil)
		return "", fmt.Errorf("unsupported command %d", req[1])
	}
	host, err := readSocks5Addr(c, req[3])
	if err != nil {
		return "", err
	}
	var port [2]byte
	if _, err := io.ReadFull(c, port[:]); err != nil {
		return "", err
	}
	return net.JoinHostPort(host, fmt.Sprint(binary.BigEndian.Uint16(port[:]))), nil
}

func readSocks5Addr(r io.Reader, atyp byte) (string, error) {
	switch atyp {
	case 0x01:
		var b [4]byte
		if _, err := io.ReadFull(r, b[:]); err != nil {
			return "", err
		}
		return net.IP(b[:]).String(), nil
	case 0x04:
		var b [16]byte
		if _, err := io.ReadFull(r, b[:]); err != nil {
			return "", err
		}
		return net.IP(b[:]).String(), nil
	case 0x03:
		var l [1]byte
		if _, err := io.ReadFull(r, l[:]); err != nil {
			return "", err
		}
		b := make([]byte, l[0])
		if _, err := io.ReadFull(r, b); err != nil {
			return "", err
		}
		return string(b), nil
	}
	return "", fmt.Errorf("unsupported address type %d", atyp)
}

func writeSocks5Reply(w io.Writer, rep byte, local net.Addr) error {
	addr := []byte{0, 0, 0, 0}
	port := [2]byte{}
	if local != nil {
		if ta, ok := local.(*net.TCPAddr); ok && ta.IP != nil {
			if ip4 := ta.IP.To4(); ip4 != nil {
				addr = ip4
			} else {
				addr = ta.IP.To16()
			}
			binary.BigEndian.PutUint16(port[:], uint16(ta.Port))
		}
	}
	buf := []byte{0x05, rep, 0x00}
	if len(addr) == 16 {
		buf = append(buf, 0x04)
	} else {
		buf = append(buf, 0x01)
	}
	buf = append(buf, addr...)
	buf = append(buf, port[:]...)
	_, err := w.Write(buf)
	return err
}

// dialSocks5 opens a CONNECT tunnel to addr through the SOCKS5 exit.
func dialSocks5(ctx context.Context, exit, addr string) (net.Conn, error) {
	d, err := xproxy.SOCKS5("tcp", exit, nil, &net.Dialer{Timeout: 15 * time.Second})
	if err != nil {
		return nil, err
	}
	cd, ok := d.(xproxy.ContextDialer)
	if !ok {
		return d.Dial("tcp", addr)
	}
	return cd.DialContext(ctx, "tcp", addr)
}

// ParseExitList accepts comma-separated "host:port" or socks5://host:port
// entries.
func ParseExitList(spec string) ([]string, error) {
	var out []string
	for _, raw := range splitComma(spec) {
		e := raw
		if i := len("socks5://"); len(e) > i && e[:i] == "socks5://" {
			u, err := url.Parse(e)
			if err != nil {
				return nil, err
			}
			e = u.Host
		}
		if e == "" {
			continue
		}
		out = append(out, e)
	}
	return out, nil
}

func splitComma(s string) []string {
	var parts []string
	start := 0
	for i := 0; i < len(s); i++ {
		if s[i] == ',' || s[i] == ' ' {
			if i > start {
				parts = append(parts, s[start:i])
			}
			start = i + 1
		}
	}
	if start < len(s) {
		parts = append(parts, s[start:])
	}
	return parts
}
