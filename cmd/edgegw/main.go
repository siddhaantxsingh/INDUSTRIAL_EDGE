// edgegw: industrial edge gateway (Modbus TCP -> analytics -> durable queue -> MQTT).
//
//	edgegw -config gateway.json
//
// SIGINT/SIGTERM: stop polling, flush compression state, drain the queue to
// the broker for up to -drain, sync and exit. Data still queued stays on
// disk and is sent after the next start.
package main

import (
	"context"
	"flag"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"industrial-edge/internal/gateway"
)

func main() {
	cfgPath := flag.String("config", "gateway.json", "configuration file")
	drain := flag.Duration("drain", 5*time.Second, "how long to drain the queue on shutdown")
	flag.Parse()
	cfg, err := gateway.LoadConfig(*cfgPath)
	if err != nil {
		log.Fatal(err)
	}
	g, err := gateway.New(cfg)
	if err != nil {
		log.Fatal(err)
	}
	log.Printf("gateway %s: %d devices, queue backlog %d (truncated %d torn bytes)", cfg.GatewayID, len(cfg.Devices),
		g.Q.Backlog(), g.Q.Truncated)
	g.Start()
	var srv *http.Server
	if cfg.HTTP != "" {
		srv = &http.Server{Addr: cfg.HTTP, Handler: g.Handler(), ReadHeaderTimeout: 5 * time.Second}
		go func() {
			if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
				log.Fatal(err)
			}
		}()
	}
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
	<-sig
	log.Printf("shutting down")
	if srv != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		srv.Shutdown(ctx)
		cancel()
	}
	g.Stop(*drain)
	log.Printf("stopped; backlog %d", g.Q.Backlog())
}
