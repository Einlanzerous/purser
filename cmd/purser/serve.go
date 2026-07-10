package main

import (
	"context"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/Einlanzerous/purser/internal/api"
	"github.com/Einlanzerous/purser/internal/version"
)

// runServe boots the HTTP API and blocks until SIGINT/SIGTERM.
func runServe() {
	ctx := context.Background()
	a, err := setup(ctx)
	if err != nil {
		log.Fatal(err)
	}
	defer a.cleanup()

	srv := &http.Server{
		Addr:              a.cfg.Addr,
		Handler:           api.New(a.svc, a.store, a.cfg.APIToken).Handler(),
		ReadHeaderTimeout: 10 * time.Second,
	}

	shutdown, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	go func() {
		log.Printf("purser %s listening on %s", version.Version, a.cfg.Addr)
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Printf("server error: %v", err)
			stop()
		}
	}()

	<-shutdown.Done()
	log.Printf("shutting down")

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		log.Printf("graceful shutdown: %v", err)
	}
}
