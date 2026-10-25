// Package sample is the bundled demo system that faultline experiments run
// against: a TCP "pricing" backend speaking a line protocol and an HTTP
// "catalog" API that calls it with a strict timeout. It is intentionally
// small but behaves like a real dependency chain, so injected network
// faults surface as user-visible errors and latency.
package sample

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Prices is the pricing backend's fixed price list, in cents.
var Prices = map[string]int{
	"anvil":   4999,
	"rocket":  129900,
	"magnet":  1250,
	"tornado": 99,
}

// Pricing is the TCP pricing backend.
//
// Protocol (one command per line):
//
//	PING          -> PONG
//	PRICE <sku>   -> OK <sku> <cents> | ERR unknown sku
type Pricing struct {
	ln  net.Listener
	log *slog.Logger
	wg  sync.WaitGroup

	mu    sync.Mutex
	conns map[net.Conn]struct{}
}

// StartPricing listens on addr and serves the pricing protocol.
func StartPricing(addr string, log *slog.Logger) (*Pricing, error) {
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	ln, err := (&net.ListenConfig{}).Listen(context.Background(), "tcp", addr)
	if err != nil {
		return nil, fmt.Errorf("pricing: listen %s: %w", addr, err)
	}
	p := &Pricing{ln: ln, log: log.With("svc", "pricing"), conns: map[net.Conn]struct{}{}}
	p.wg.Add(1)
	go p.serve()
	return p, nil
}

// Addr is the listen address.
func (p *Pricing) Addr() string { return p.ln.Addr().String() }

// Close stops the server and closes open connections.
func (p *Pricing) Close() error {
	err := p.ln.Close()
	p.mu.Lock()
	for c := range p.conns {
		_ = c.Close()
	}
	p.mu.Unlock()
	p.wg.Wait()
	return err
}

func (p *Pricing) serve() {
	defer p.wg.Done()
	for {
		c, err := p.ln.Accept()
		if err != nil {
			return
		}
		p.mu.Lock()
		p.conns[c] = struct{}{}
		p.mu.Unlock()
		p.wg.Add(1)
		go func() {
			defer p.wg.Done()
			p.handle(c)
			p.mu.Lock()
			delete(p.conns, c)
			p.mu.Unlock()
		}()
	}
}

func (p *Pricing) handle(c net.Conn) {
	defer c.Close()
	r := bufio.NewReader(c)
	for {
		_ = c.SetReadDeadline(time.Now().Add(30 * time.Second))
		line, err := r.ReadString('\n')
		if err != nil {
			return
		}
		if _, err := io.WriteString(c, Answer(strings.TrimSpace(line))+"\n"); err != nil {
			return
		}
	}
}

// Answer computes the pricing protocol reply to one command.
func Answer(cmd string) string {
	fields := strings.Fields(cmd)
	switch {
	case len(fields) == 1 && fields[0] == "PING":
		return "PONG"
	case len(fields) == 2 && fields[0] == "PRICE":
		if cents, ok := Prices[fields[1]]; ok {
			return fmt.Sprintf("OK %s %d", fields[1], cents)
		}
		return "ERR unknown sku"
	}
	return "ERR bad command"
}

// CatalogConfig configures the catalog API.
type CatalogConfig struct {
	Listen         string
	PricingAddr    string
	PricingTimeout time.Duration
	Logger         *slog.Logger
}

// Product is one catalog entry.
type Product struct {
	SKU        string `json:"sku"`
	PriceCents int    `json:"price_cents"`
}

// Catalog is the HTTP API in front of the pricing backend.
type Catalog struct {
	cfg  CatalogConfig
	ln   net.Listener
	srv  *http.Server
	log  *slog.Logger
	done chan struct{}
}

// StartCatalog starts the catalog API.
func StartCatalog(cfg CatalogConfig) (*Catalog, error) {
	if cfg.PricingAddr == "" {
		return nil, errors.New("catalog: pricing address is required")
	}
	if cfg.PricingTimeout <= 0 {
		cfg.PricingTimeout = 250 * time.Millisecond
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.New(slog.DiscardHandler)
	}
	ln, err := (&net.ListenConfig{}).Listen(context.Background(), "tcp", cfg.Listen)
	if err != nil {
		return nil, fmt.Errorf("catalog: listen %s: %w", cfg.Listen, err)
	}
	c := &Catalog{cfg: cfg, ln: ln, log: cfg.Logger.With("svc", "catalog"), done: make(chan struct{})}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, "ok\n")
	})
	mux.HandleFunc("GET /products", c.products)
	mux.HandleFunc("GET /products/{sku}", c.product)
	c.srv = &http.Server{Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	go func() {
		defer close(c.done)
		if err := c.srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			c.log.Error("catalog stopped", "err", err)
		}
	}()
	return c, nil
}

// Addr is the listen address.
func (c *Catalog) Addr() string { return c.ln.Addr().String() }

// Close stops the API.
func (c *Catalog) Close() error {
	err := c.srv.Close()
	<-c.done
	return err
}

func (c *Catalog) products(w http.ResponseWriter, r *http.Request) {
	skus := make([]string, 0, len(Prices))
	for s := range Prices {
		skus = append(skus, s)
	}
	sort.Strings(skus)
	c.respond(w, r, skus)
}

func (c *Catalog) product(w http.ResponseWriter, r *http.Request) {
	c.respond(w, r, []string{r.PathValue("sku")})
}

func (c *Catalog) respond(w http.ResponseWriter, r *http.Request, skus []string) {
	products, err := c.lookup(r.Context(), skus)
	w.Header().Set("Content-Type", "application/json")
	var unknown errUnknownSKU
	switch {
	case errors.As(err, &unknown):
		w.WriteHeader(http.StatusNotFound)
		_ = json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
	case err != nil:
		c.log.Warn("pricing lookup failed", "err", err)
		w.WriteHeader(http.StatusServiceUnavailable)
		_ = json.NewEncoder(w).Encode(map[string]string{"error": "pricing unavailable: " + err.Error()})
	default:
		_ = json.NewEncoder(w).Encode(map[string]any{"products": products})
	}
}

type errUnknownSKU string

func (e errUnknownSKU) Error() string { return "unknown sku " + string(e) }

// lookup opens one connection to pricing, pipelines every PRICE command and
// reads the replies, all under PricingTimeout.
func (c *Catalog) lookup(ctx context.Context, skus []string) ([]Product, error) {
	ctx, cancel := context.WithTimeout(ctx, c.cfg.PricingTimeout)
	defer cancel()
	var d net.Dialer
	conn, err := d.DialContext(ctx, "tcp", c.cfg.PricingAddr)
	if err != nil {
		return nil, fmt.Errorf("dial: %w", err)
	}
	defer conn.Close()
	dl, _ := ctx.Deadline()
	_ = conn.SetDeadline(dl)
	var req strings.Builder
	for _, s := range skus {
		fmt.Fprintf(&req, "PRICE %s\n", s)
	}
	if _, err := io.WriteString(conn, req.String()); err != nil {
		return nil, fmt.Errorf("write: %w", err)
	}
	rd := bufio.NewReader(conn)
	out := make([]Product, 0, len(skus))
	for _, s := range skus {
		line, err := rd.ReadString('\n')
		if err != nil {
			var ne net.Error
			if errors.As(err, &ne) && ne.Timeout() {
				return nil, errors.New("timeout")
			}
			return nil, fmt.Errorf("read: %w", err)
		}
		f := strings.Fields(line)
		if len(f) == 3 && f[0] == "ERR" {
			return nil, errUnknownSKU(s)
		}
		if len(f) != 3 || f[0] != "OK" || f[1] != s {
			return nil, fmt.Errorf("malformed reply %q", strings.TrimSpace(line))
		}
		cents, err := strconv.Atoi(f[2])
		if err != nil {
			return nil, fmt.Errorf("malformed price %q", f[2])
		}
		out = append(out, Product{SKU: s, PriceCents: cents})
	}
	return out, nil
}
