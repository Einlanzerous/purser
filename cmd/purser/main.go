// Command purser is the cross-service provisioning/invite service: one action
// invites a person into multiple Construct services at once, mints starter
// credentials, grants Cloudflare Access SSO, and hands back a copy-pasteable
// credential block (or emails it).
//
// It is a single static binary that is both a CLI and a thin HTTP API:
//
//	purser                      # run the HTTP server (default)
//	purser serve                # ditto
//	purser invite --name … --email … --to switchyard,cloudflare
//	purser migrate              # apply DB migrations and exit
//	purser version              # print the build version
//
// It is a sibling to the other construct-server Go services (Lyceum, Argosy):
// shared Postgres 16, embedded auto-migrations, construct_net networking.
package main

import (
	"context"
	"fmt"
	"log"
	"os"

	"github.com/Einlanzerous/purser/internal/config"
	"github.com/Einlanzerous/purser/internal/connector"
	"github.com/Einlanzerous/purser/internal/connectors/argosy"
	"github.com/Einlanzerous/purser/internal/connectors/cloudflare"
	"github.com/Einlanzerous/purser/internal/connectors/switchyard"
	"github.com/Einlanzerous/purser/internal/delivery"
	"github.com/Einlanzerous/purser/internal/invite"
	"github.com/Einlanzerous/purser/internal/store"
	"github.com/Einlanzerous/purser/internal/version"
)

func main() {
	log.SetFlags(log.LstdFlags | log.Lmsgprefix)
	log.SetPrefix("purser: ")

	cmd := "serve"
	args := os.Args[1:]
	if len(args) > 0 && !isFlag(args[0]) {
		cmd, args = args[0], args[1:]
	}

	switch cmd {
	case "serve":
		runServe()
	case "invite":
		runInvite(args)
	case "migrate":
		runMigrate()
	case "version":
		fmt.Println(version.Version)
	default:
		fmt.Fprintf(os.Stderr, "purser: unknown command %q\n", cmd)
		fmt.Fprintln(os.Stderr, "commands: serve, invite, migrate, version")
		os.Exit(2)
	}
}

func isFlag(s string) bool { return len(s) > 0 && s[0] == '-' }

// app bundles the wired-up dependencies shared by the subcommands.
type app struct {
	cfg     config.Config
	store   *store.Store
	svc     *invite.Service
	cleanup func()
}

// setup connects the store, applies migrations, seeds the service table from the
// connector registry, and builds the invite orchestrator.
func setup(ctx context.Context) (*app, error) {
	cfg := config.Load()

	pool, err := store.Connect(ctx, cfg.DatabaseURL)
	if err != nil {
		return nil, fmt.Errorf("connect database: %w", err)
	}
	if err := store.Migrate(ctx, pool); err != nil {
		pool.Close()
		return nil, fmt.Errorf("migrate: %w", err)
	}
	st := store.New(pool)

	registry := buildRegistry(cfg)
	for _, c := range registry.All() {
		if _, err := st.EnsureService(ctx, c.Key(), c.DisplayName()); err != nil {
			pool.Close()
			return nil, fmt.Errorf("seed service %q: %w", c.Key(), err)
		}
	}

	var emailer invite.Emailer
	if cfg.SMTP.Configured() {
		sender, err := delivery.New(delivery.Config{
			Host: cfg.SMTP.Host, Port: cfg.SMTP.Port,
			Username: cfg.SMTP.Username, Password: cfg.SMTP.Password,
			From: cfg.SMTP.From, TLS: cfg.SMTP.TLS,
		})
		if err != nil {
			pool.Close()
			return nil, fmt.Errorf("configure SMTP: %w", err)
		}
		emailer = sender
		log.Printf("email delivery enabled via %s", cfg.SMTP.Host)
	}

	svc := invite.New(st, registry, emailer)
	return &app{cfg: cfg, store: st, svc: svc, cleanup: pool.Close}, nil
}

// buildRegistry wires the connectors from config. Switchyard needs a token, so
// it degrades to an Unavailable connector when unconfigured; Cloudflare and
// Argosy are always registered (Cloudflare self-degrades to manual
// instructions, Argosy is pending an upstream endpoint).
func buildRegistry(cfg config.Config) *connector.Registry {
	var conns []connector.Connector

	if cfg.Switchyard.Configured() {
		sc, err := switchyard.New(switchyard.Config{
			BaseURL:  cfg.Switchyard.BaseURL,
			Token:    cfg.Switchyard.Token,
			LoginURL: cfg.Switchyard.LoginURL,
		})
		if err != nil {
			log.Fatalf("switchyard connector: %v", err)
		}
		conns = append(conns, sc)
	} else {
		conns = append(conns, connector.NewUnavailable("switchyard", "Switchyard",
			"set PURSER_SWITCHYARD_TOKEN (and PURSER_SWITCHYARD_BASE_URL) to enable"))
	}

	conns = append(conns, cloudflare.New(cloudflare.Config{
		APIToken:   cfg.Cloudflare.APIToken,
		AccountID:  cfg.Cloudflare.AccountID,
		GroupID:    cfg.Cloudflare.GroupID,
		GroupName:  cfg.Cloudflare.GroupName,
		TeamDomain: cfg.Cloudflare.TeamDomain,
		AppsNote:   cfg.Cloudflare.AppsNote,
	}))

	conns = append(conns, argosy.New())

	return connector.NewRegistry(conns...)
}

func runMigrate() {
	ctx := context.Background()
	pool, err := store.Connect(ctx, config.Load().DatabaseURL)
	if err != nil {
		log.Fatalf("connect database: %v", err)
	}
	defer pool.Close()
	if err := store.Migrate(ctx, pool); err != nil {
		log.Fatalf("migrate: %v", err)
	}
	log.Printf("migrations applied")
}
