// Command nd-simple-rpc posts Navidrome playback to Discord as a headless-session activity.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/laforcem/nd-simple-rpc/internal/api"
	"github.com/laforcem/nd-simple-rpc/internal/config"
	"github.com/laforcem/nd-simple-rpc/internal/discord"
	"github.com/laforcem/nd-simple-rpc/internal/engine"
	"github.com/laforcem/nd-simple-rpc/internal/install"
	"github.com/laforcem/nd-simple-rpc/internal/store"
	"github.com/laforcem/nd-simple-rpc/internal/tokens"
	"github.com/laforcem/nd-simple-rpc/internal/web"
)

// version is set at build time with -ldflags "-X main.version=...".
var version = "dev"

const (
	usage      = "usage: nd-simple-rpc [serve | install-plugin [--src FILE] DIR]\n"
	defaultSrc = "/plugin/nd-simple-rpc.ndp"
)

func main() {
	discord.Version = version
	cmd := "serve"
	if len(os.Args) > 1 {
		cmd = os.Args[1]
	}
	switch cmd {
	case "serve":
		log := slog.New(slog.NewJSONHandler(os.Stderr, nil))
		slog.SetDefault(log)
		if err := serve(log); err != nil {
			log.Error("fatal", "err", err)
			os.Exit(1)
		}
	case "install-plugin":
		if err := installPlugin(os.Args[2:]); err != nil {
			fmt.Fprintln(os.Stderr, "install-plugin:", err)
			os.Exit(1)
		}
	default:
		fmt.Fprint(os.Stderr, usage)
		os.Exit(2)
	}
}

func installPlugin(args []string) error {
	fs := flag.NewFlagSet("install-plugin", flag.ContinueOnError)
	src := fs.String("src", defaultSrc, "plugin file to install")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 1 {
		return errors.New("want exactly one destination directory")
	}
	changed, err := install.Plugin(*src, fs.Arg(0))
	if err != nil {
		return err
	}
	if changed {
		fmt.Println("installed", install.FileName, "into", fs.Arg(0))
	} else {
		fmt.Println(install.FileName, "already up to date; nothing changed")
	}
	return nil
}

func serve(log *slog.Logger) error {
	cfg, err := config.Load(os.Getenv)
	if err != nil {
		return err
	}
	st, err := store.Open(cfg.DBPath, cfg.EncryptionKey)
	if err != nil {
		return fmt.Errorf("open store: %w", err)
	}
	defer st.Close()

	dc := &discord.Client{
		ClientID:     cfg.ClientID,
		ClientSecret: cfg.ClientSecret,
		RedirectURI:  cfg.PublicURL + "/callback",
	}
	tm := &tokens.Manager{Store: st, Discord: dc, Now: time.Now}
	eng := engine.New(engine.Config{
		AppID:      cfg.ClientID,
		Keepalive:  15 * time.Minute,
		Grace:      30 * time.Second,
		Tolerance:  3 * time.Second,
		RateMax:    4,
		RateWindow: 20 * time.Second,
		Logger:     log,
	}, st, tm, dc, time.Now)

	pub, err := web.NewPublic(web.Deps{
		Store:        st,
		Discord:      dc,
		Engine:       eng,
		NavidromeURL: cfg.NavidromeURL,
		PublicURL:    cfg.PublicURL,
		SessionKey:   cfg.SessionKey,
		Log:          log,
	})
	if err != nil {
		return err
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, os.Interrupt)
	defer stop()

	if err := eng.Startup(ctx); err != nil {
		return fmt.Errorf("engine startup: %w", err)
	}
	runDone := make(chan struct{})
	go func() { eng.Run(ctx); close(runDone) }()

	srvs := []*http.Server{
		{Addr: cfg.PublicAddr, Handler: pub, ReadHeaderTimeout: 10 * time.Second, ReadTimeout: 30 * time.Second, WriteTimeout: 30 * time.Second, IdleTimeout: 2 * time.Minute},
		{Addr: cfg.InternalAddr, Handler: api.NewInternal(cfg.SharedSecret, eng), ReadHeaderTimeout: 10 * time.Second, ReadTimeout: 30 * time.Second, WriteTimeout: 30 * time.Second, IdleTimeout: 2 * time.Minute},
	}
	errc := make(chan error, len(srvs))
	for _, s := range srvs {
		go func() {
			log.Info("listening", "addr", s.Addr)
			errc <- s.ListenAndServe()
		}()
	}

	var runErr error
	select {
	case <-ctx.Done():
		log.Info("shutting down")
	case runErr = <-errc:
		stop()
	}
	sctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	for _, s := range srvs {
		if err := s.Shutdown(sctx); err != nil {
			log.Error("shutdown", "addr", s.Addr, "err", err)
		}
	}
	<-runDone
	if errors.Is(runErr, http.ErrServerClosed) {
		runErr = nil
	}
	return runErr
}
