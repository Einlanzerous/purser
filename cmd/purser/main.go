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
//	purser offboard --email …   # revoke access; previews unless --apply
//	purser provision-service --service … --hostname …  # stand up a service's edge
//	purser person add --name … --email …  # record someone; provisions nothing
//	purser person list          # the roster: who has what, from local records
//	purser person show --email … # one person in full, from local records
//	purser audit                # report record-vs-upstream drift (read-only)
//	purser reconcile --email …  # repair the records; never mints a credential
//	purser migrate              # apply DB migrations and exit
//	purser version              # print the build version
//
// It is a sibling to the other construct-server Go services (Lyceum, Argosy):
// shared Postgres 16, embedded auto-migrations, construct_net networking.
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"os"
	"time"

	"github.com/Einlanzerous/purser/internal/config"
	"github.com/Einlanzerous/purser/internal/connector"
	"github.com/Einlanzerous/purser/internal/connectors/argosy"
	"github.com/Einlanzerous/purser/internal/connectors/cloudflare"
	"github.com/Einlanzerous/purser/internal/connectors/lyceum"
	"github.com/Einlanzerous/purser/internal/connectors/switchyard"
	"github.com/Einlanzerous/purser/internal/delivery"
	"github.com/Einlanzerous/purser/internal/invite"
	"github.com/Einlanzerous/purser/internal/spinup"
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
	case "offboard":
		runOffboard(args)
	case "provision-service":
		runProvisionService(args)
	case "person":
		runPerson(args)
	case "audit":
		runAudit(args, false)
	case "reconcile":
		runAudit(args, true)
	case "migrate":
		runMigrate()
	case "version":
		fmt.Println(version.Get().Version)
	default:
		fmt.Fprintf(os.Stderr, "purser: unknown command %q\n", cmd)
		fmt.Fprintln(os.Stderr, "commands: serve, invite, offboard, provision-service, person, audit, reconcile, migrate, version")
		os.Exit(2)
	}
}

func isFlag(s string) bool { return len(s) > 0 && s[0] == '-' }

// requireNoOperands refuses leftover positional arguments.
//
// Go's flag package stops parsing at the first operand and leaves the rest in
// Args(), so `purser person list switchyard --to lyceum` silently discards
// --to and answers a different question than the one asked — with exit 0, which
// is the worst way for a roster command to be wrong. Nothing here takes a
// positional argument, so any leftover is a mistake worth naming.
func requireNoOperands(fs *flag.FlagSet, usage string) {
	if fs.NArg() == 0 {
		return
	}
	fmt.Fprintf(os.Stderr, "purser: unexpected argument %q — every option is a flag\n", fs.Arg(0))
	fmt.Fprintln(os.Stderr, usage)
	os.Exit(2)
}

// app bundles the wired-up dependencies shared by the subcommands.
type app struct {
	cfg   config.Config
	store *store.Store
	// svc is the person axis: invite, offboard, audit, reconcile.
	svc *invite.Service
	// spin is the service spin-up axis (PRSR-22). A second orchestrator rather
	// than a widened first one, for the reason internal/spinup exists at all:
	// one is keyed on a person, the other on a hostname, and they share no
	// types. The composition root is where they meet, and it is the only place
	// they do.
	spin    *spinup.Service
	cleanup func()
}

// setup connects the store, applies migrations, seeds the service table from the
// connector registry, and builds the invite orchestrator.
func setup(ctx context.Context) (*app, error) {
	cfg := config.Load()

	pool, err := store.ConnectWithRetry(ctx, cfg.DatabaseURL, 60*time.Second)
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

	svc := invite.New(st, registry, emailer,
		invite.WithBundles(bundleSet(cfg)),
		invite.WithLauncher(cfg.Cloudflare.LauncherURL()),
	)
	spin := spinup.New(st, spinupRegistry(cfg), spinup.WithTunnels(tunnelSet(cfg)))
	return &app{cfg: cfg, store: st, svc: svc, spin: spin, cleanup: pool.Close}, nil
}

// spinupRegistry wires the three edge provisioners (PRSR-28/29/30).
//
// All three are registered unconditionally, which is the opposite of
// buildRegistry above and deliberate. There, an unconfigured connector is
// replaced by connector.Unavailable because the connector itself would fail to
// construct. Here each provisioner constructs fine without credentials and
// reports spinup.ErrUnavailable from every method — naming the environment
// variable that is missing, which a generic stand-in could not. Registering them
// anyway also keeps the kind *known*: spinup.NewRegistry panics on a kind
// outside model.KindOrder precisely so a step can never be silently absent from
// a report, and "no provisioner for dns_record" reads like a missing build where
// "set PURSER_CF_ZONE_ID" reads like the thing to go and do.
//
// One token serves all three (PRSR-11 probed both scopes), and one API client
// per provisioner: they are grouped by upstream, not by axis.
func spinupRegistry(cfg config.Config) *spinup.Registry {
	return spinup.NewRegistry(
		cloudflare.NewDNS(cloudflare.DNSConfig{
			APIToken: cfg.Cloudflare.APIToken,
			ZoneID:   cfg.Cloudflare.ZoneID,
		}),
		cloudflare.NewAccess(cloudflare.AccessConfig{
			APIToken:  cfg.Cloudflare.APIToken,
			AccountID: cfg.Cloudflare.AccountID,
			GroupID:   cfg.Cloudflare.GroupID,
			GroupName: cfg.Cloudflare.GroupName,
		}),
		cloudflare.NewTunnel(cloudflare.TunnelConfig{
			APIToken:  cfg.Cloudflare.APIToken,
			AccountID: cfg.Cloudflare.AccountID,
		}),
	)
}

// tunnelSet maps the spec-facing tunnel refs to the ids they stand for, so
// internal/spinup reads no environment — the arrangement bundleSet has.
//
// Only prod is wired. `dev` is a name a spec may legally use and resolves to a
// clear refusal until PRSR-33 supplies its id, which is the whole reason a spec
// names a ref rather than carrying an opaque id: adding the second tunnel is a
// line here, not a fork of everything downstream. Mapping it to prod's id as a
// fallback would be the one outcome worth preventing — a dev spin-up quietly
// rewriting the production tunnel's shared ingress document.
func tunnelSet(cfg config.Config) spinup.TunnelSet {
	return spinup.TunnelSet{spinup.TunnelProd: cfg.Cloudflare.TunnelID}
}

// bundleSet converts the configured onboarding bundles into the orchestrator's
// form (PRSR-12). Kept here in the composition root so internal/config stays
// dependency-free and the invite package doesn't read the environment.
func bundleSet(cfg config.Config) invite.BundleSet {
	bs := invite.BundleSet{
		Named:   make(map[string]invite.Bundle, len(cfg.Bundles.Named)),
		Default: cfg.Bundles.Default,
	}
	for name, b := range cfg.Bundles.Named {
		bs.Named[name] = invite.Bundle{Services: b.Services, Projects: b.Projects}
	}
	return bs
}

// buildRegistry wires the connectors from config. Switchyard, Lyceum and Argosy
// each need a token, so they degrade to an Unavailable connector when
// unconfigured; Cloudflare is always registered and self-degrades to printing
// the manual dashboard step.
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

	if cfg.Argosy.Configured() {
		ac, err := argosy.New(argosy.Config{
			BaseURL:        cfg.Argosy.BaseURL,
			ProvisionToken: cfg.Argosy.ProvisionToken,
			AppURL:         cfg.Argosy.AppURL,
		})
		if err != nil {
			log.Fatalf("argosy connector: %v", err)
		}
		conns = append(conns, ac)
	} else {
		conns = append(conns, connector.NewUnavailable("argosy", "Argosy",
			"set PURSER_ARGOSY_PROVISION_TOKEN (matching the argosy service's ARGOSY_PROVISION_TOKEN) to enable"))
	}

	if cfg.Lyceum.Configured() {
		lc, err := lyceum.New(lyceum.Config{
			BaseURL:    cfg.Lyceum.BaseURL,
			OwnerToken: cfg.Lyceum.OwnerToken,
			AppURL:     cfg.Lyceum.AppURL,
		})
		if err != nil {
			log.Fatalf("lyceum connector: %v", err)
		}
		conns = append(conns, lc)
	} else {
		conns = append(conns, connector.NewUnavailable("lyceum", "Lyceum",
			"set PURSER_LYCEUM_OWNER_TOKEN (owner session token) and run the lyceum service with LYCEUM_AUTH=true"))
	}

	return connector.NewRegistry(conns...)
}

func runMigrate() {
	ctx := context.Background()
	pool, err := store.ConnectWithRetry(ctx, config.Load().DatabaseURL, 60*time.Second)
	if err != nil {
		log.Fatalf("connect database: %v", err)
	}
	defer pool.Close()
	if err := store.Migrate(ctx, pool); err != nil {
		log.Fatalf("migrate: %v", err)
	}
	log.Printf("migrations applied")
}
