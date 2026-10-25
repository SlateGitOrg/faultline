package sample

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"
)

func TestAnswer(t *testing.T) {
	cases := map[string]string{
		"PING":           "PONG",
		"PRICE anvil":    "OK anvil 4999",
		"PRICE unobtain": "ERR unknown sku",
		"DANCE":          "ERR bad command",
		"":               "ERR bad command",
	}
	for in, want := range cases {
		if got := Answer(in); got != want {
			t.Fatalf("Answer(%q)=%q want %q", in, got, want)
		}
	}
}

func start(t *testing.T, timeout time.Duration) (*Pricing, *Catalog) {
	t.Helper()
	p, err := StartPricing("127.0.0.1:0", nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = p.Close() })
	c, err := StartCatalog(CatalogConfig{Listen: "127.0.0.1:0", PricingAddr: p.Addr(), PricingTimeout: timeout})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return p, c
}

func getJSON(t *testing.T, url string) (int, map[string]any) {
	t.Helper()
	resp, err := http.Get(url) //nolint:noctx // test helper
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var body map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&body)
	return resp.StatusCode, body
}

func TestCatalogEndToEnd(t *testing.T) {
	_, c := start(t, 0)
	code, body := getJSON(t, "http://"+c.Addr()+"/products")
	if code != 200 {
		t.Fatalf("status %d %v", code, body)
	}
	products := body["products"].([]any)
	if len(products) != len(Prices) {
		t.Fatalf("products: %v", products)
	}
	first := products[0].(map[string]any)
	if first["sku"] != "anvil" || first["price_cents"].(float64) != 4999 {
		t.Fatalf("first product %v", first)
	}
	code, body = getJSON(t, "http://"+c.Addr()+"/products/magnet")
	if code != 200 || len(body["products"].([]any)) != 1 {
		t.Fatalf("single: %d %v", code, body)
	}
	if code, _ := getJSON(t, "http://"+c.Addr()+"/products/unobtainium"); code != 404 {
		t.Fatalf("unknown sku status %d", code)
	}
	resp, err := http.Get("http://" + c.Addr() + "/healthz") //nolint:noctx // test
	if err != nil || resp.StatusCode != 200 {
		t.Fatalf("healthz %v %v", resp, err)
	}
	_ = resp.Body.Close()
}

func TestCatalogPricingDown(t *testing.T) {
	p, c := start(t, 100*time.Millisecond)
	_ = p.Close()
	code, body := getJSON(t, "http://"+c.Addr()+"/products")
	if code != 503 || !strings.Contains(fmt.Sprint(body["error"]), "pricing unavailable") {
		t.Fatalf("got %d %v", code, body)
	}
}

// slowPricing answers after a delay, to exercise the catalog's timeout.
func TestCatalogPricingTimeout(t *testing.T) {
	ln, _ := net.Listen("tcp", "127.0.0.1:0")
	defer ln.Close()
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				defer conn.Close()
				_, _ = bufio.NewReader(conn).ReadString('\n')
				time.Sleep(300 * time.Millisecond)
				_, _ = io.WriteString(conn, "OK anvil 1\n")
			}()
		}
	}()
	c, err := StartCatalog(CatalogConfig{Listen: "127.0.0.1:0", PricingAddr: ln.Addr().String(), PricingTimeout: 50 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	start := time.Now()
	code, body := getJSON(t, "http://"+c.Addr()+"/products/anvil")
	if code != 503 || body["error"] != "pricing unavailable: timeout" || time.Since(start) > 250*time.Millisecond {
		t.Fatalf("got %d %v after %v", code, body, time.Since(start))
	}
}

func TestMalformedReplies(t *testing.T) {
	for _, reply := range []string{"WAT\n", "OK other 5\n", "OK anvil x\n"} {
		ln, _ := net.Listen("tcp", "127.0.0.1:0")
		go func() {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			defer conn.Close()
			_, _ = bufio.NewReader(conn).ReadString('\n')
			_, _ = io.WriteString(conn, reply)
		}()
		c, err := StartCatalog(CatalogConfig{Listen: "127.0.0.1:0", PricingAddr: ln.Addr().String()})
		if err != nil {
			t.Fatal(err)
		}
		code, body := getJSON(t, "http://"+c.Addr()+"/products/anvil")
		if code != 503 || !strings.Contains(fmt.Sprint(body["error"]), "malformed") {
			t.Fatalf("reply %q: %d %v", reply, code, body)
		}
		_ = c.Close()
		_ = ln.Close()
	}
}

func TestStartErrors(t *testing.T) {
	if _, err := StartCatalog(CatalogConfig{Listen: "127.0.0.1:0"}); err == nil {
		t.Fatal("missing pricing addr")
	}
	if _, err := StartCatalog(CatalogConfig{Listen: "256.0.0.1:0", PricingAddr: "x:1"}); err == nil {
		t.Fatal("bad listen")
	}
	if _, err := StartPricing("256.0.0.1:0", nil); err == nil {
		t.Fatal("bad listen")
	}
}

func TestPricingPipelining(t *testing.T) {
	p, _ := start(t, 0)
	conn, err := net.Dial("tcp", p.Addr())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	_, _ = io.WriteString(conn, "PING\nPRICE rocket\nPRICE tornado\n")
	r := bufio.NewReader(conn)
	for _, want := range []string{"PONG\n", "OK rocket 129900\n", "OK tornado 99\n"} {
		got, err := r.ReadString('\n')
		if err != nil || got != want {
			t.Fatalf("got %q %v want %q", got, err, want)
		}
	}
}
