// Command sampleservice runs the bundled demo system that faultline
// experiments target: a TCP pricing backend and an HTTP catalog API that
// calls it. Point --pricing-addr at a faultline TCP proxy to put the
// catalog's dependency under fault injection.
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/SlateGitOrg/faultline/internal/sample"
)

func main() {
	pricingListen := flag.String("pricing-listen", "127.0.0.1:7101", "pricing backend (TCP line protocol) listen address")
	catalogListen := flag.String("catalog-listen", "127.0.0.1:7100", "catalog HTTP API listen address")
	pricingAddr := flag.String("pricing-addr", "", "address the catalog dials for pricing (default: --pricing-listen); set it to a faultline proxy")
	timeout := flag.Duration("pricing-timeout", 250*time.Millisecond, "catalog's deadline for a pricing lookup")
	flag.Parse()

	log := slog.New(slog.NewTextHandler(os.Stderr, nil))
	if err := run(log, *pricingListen, *catalogListen, *pricingAddr, *timeout); err != nil {
		fmt.Fprintln(os.Stderr, "sampleservice:", err)
		os.Exit(1)
	}
}

func run(log *slog.Logger, pricingListen, catalogListen, pricingAddr string, timeout time.Duration) error {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	pricing, err := sample.StartPricing(pricingListen, log)
	if err != nil {
		return err
	}
	defer pricing.Close()
	if pricingAddr == "" {
		pricingAddr = pricing.Addr()
	}
	catalog, err := sample.StartCatalog(sample.CatalogConfig{
		Listen: catalogListen, PricingAddr: pricingAddr, PricingTimeout: timeout, Logger: log,
	})
	if err != nil {
		return err
	}
	defer catalog.Close()
	log.Info("sample service up", "pricing", pricing.Addr(), "catalog", "http://"+catalog.Addr(), "catalog_dials_pricing_at", pricingAddr, "pricing_timeout", timeout)
	<-ctx.Done()
	log.Info("sample service stopping")
	return nil
}
