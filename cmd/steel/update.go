package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"

	"github.com/spiiritual/steel/internal/update"
)

// subcommand runs `steel config`, `steel login`, `steel update` and the rest
// if args name one, reporting whether it did.
func subcommand(args []string) (bool, error) {
	if len(args) == 0 {
		return false, nil
	}
	switch args[0] {
	case "config":
		return true, runConfig(args[1:])
	case "update":
		return true, runUpdate(args[1:])
	case "rollback":
		return true, runRollback()
	case "updater":
		return true, runUpdater(args[1:])
	case "usage":
		return true, runUsage(args[1:], os.Stdout)
	case "login":
		return true, runLogin(args[1:])
	case "logout":
		return true, runLogout(args[1:])
	}
	return false, nil
}

func runUpdate(args []string) error {
	fs := flag.NewFlagSet("update", flag.ExitOnError)
	check := fs.Bool("check", false, "only report whether a newer release exists")
	fs.Usage = func() {
		fmt.Fprintln(fs.Output(), "usage: steel update [-check]\n\nInstalls the latest signed release over this binary, keeping the current one for `steel rollback`.")
		fs.PrintDefaults()
	}
	fs.Parse(args)
	if err := notInContainer(); err != nil {
		return err
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	u := update.New()
	r, err := u.Latest(ctx)
	if err != nil {
		return err
	}
	cur := versionString()
	if !update.Newer(cur, r.Version) {
		fmt.Printf("steel %s is up to date (latest release: %s)\n", cur, r.Version)
		return nil
	}
	if *check {
		fmt.Printf("update available: %s → %s\n", cur, r.Version)
		return nil
	}
	exe, err := executable()
	if err != nil {
		return err
	}
	if err := u.Install(ctx, r, exe); err != nil {
		return err
	}
	fmt.Printf("updated %s: %s → %s (signature and checksum verified)\n", exe, cur, r.Version)
	fmt.Println("Restart steel to run it; a running steel finishes its work on SIGTERM. `steel rollback` restores the previous version.")
	return nil
}

func runRollback() error {
	if err := notInContainer(); err != nil {
		return err
	}
	exe, err := executable()
	if err != nil {
		return err
	}
	if err := update.Rollback(exe); err != nil {
		return err
	}
	fmt.Printf("restored the previous %s; run `steel rollback` again to undo. Restart steel to run it.\n", exe)
	return nil
}

// notInContainer refuses to replace the binary inside steel's image, which
// sets STEEL_CONTAINER: the next container would start from the image's
// binary again, so the update would quietly undo itself.
func notInContainer() error {
	if os.Getenv("STEEL_CONTAINER") == "" {
		return nil
	}
	return errors.New("steel runs in a container here, so it updates with its image: send /update in Telegram if compose.yaml has the updater service, or on the host run `docker compose pull && docker compose up -d`")
}

// executable is this binary's real path: updates replace the file, not a
// symlink to it.
func executable() (string, error) {
	exe, err := os.Executable()
	if err != nil {
		return "", err
	}
	return filepath.EvalSymlinks(exe)
}
