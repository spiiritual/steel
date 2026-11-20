package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/spiiritual/steel/internal/update"
	"github.com/spiiritual/steel/internal/updater"
)

// runUpdater runs `steel updater`: the sidecar that replaces steel's
// container with a new release when steel asks (see compose.yaml).
func runUpdater(args []string) error {
	fs := flag.NewFlagSet("updater", flag.ExitOnError)
	listen := fs.String("listen", ":8086", "address to serve the API on; keep it off the host's ports")
	dockerHost := fs.String("docker", "unix:///var/run/docker.sock", "Docker Engine address")
	image := fs.String("image", "ghcr.io/spiiritual/steel", "repository releases are pulled from")
	target := fs.String("container", "", "steel's container (default: the steel service in the updater's compose project)")
	health := fs.Duration("health", time.Minute, "how long a new version must stay up before it's kept")
	fs.Usage = func() {
		fmt.Fprintln(fs.Output(), "usage: steel updater [flags]\n\nReplaces steel's container with the newest signed release, or the previous\none, when steel asks. Runs beside steel with the Docker socket; see compose.yaml.")
		fs.PrintDefaults()
	}
	fs.Parse(args)

	logf := func(format string, args ...any) {
		fmt.Fprintf(os.Stderr, "steel updater: "+format+"\n", args...)
	}
	u, err := updater.New(updater.Options{
		Docker:    *dockerHost,
		Image:     *image,
		Container: *target,
		Health:    *health,
		Releases:  updater.SignedReleases{Updater: update.New()},
		Logf:      logf,
	})
	if err != nil {
		return err
	}
	srv := &http.Server{Addr: *listen, Handler: u.Handler(), ReadHeaderTimeout: 10 * time.Second}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	go func() {
		<-ctx.Done()
		shut, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		srv.Shutdown(shut)
	}()
	logf("%s, serving on %s", versionString(), *listen)
	if err := srv.ListenAndServe(); !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}
