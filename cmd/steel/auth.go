package main

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strings"

	"github.com/spiiritual/steel/internal/anthropicauth"
)

const loginUsage = `usage: steel login [-config path]

Starts a Claude Pro/Max OAuth login. steel prints a URL; open it in any
browser, sign in, and paste the code Anthropic shows back here. Tokens are
saved in .env beside config.json, with steel's other tokens, and refreshed
automatically.

Afterwards: steel config set auth claude-oauth, then set a Claude model.
In a container without a usable browser, /login on Telegram does the same.`

func runLogin(args []string) error {
	fs := flag.NewFlagSet("login", flag.ExitOnError)
	configPath := fs.String("config", defaultConfigPath(), "settings file; tokens are saved in .env beside it")
	fs.Usage = func() {
		fmt.Fprintln(fs.Output(), loginUsage)
		fs.PrintDefaults()
	}
	fs.Parse(args)
	if fs.NArg() != 0 {
		fs.Usage()
		os.Exit(2)
	}
	path := oauthPath(*configPath)
	login, err := anthropicauth.NewLogin(anthropicauth.Default(), path)
	if err != nil {
		return err
	}
	fmt.Println("Open this URL in a browser on any machine, sign in, and paste the code Anthropic shows:")
	fmt.Println()
	fmt.Println(login.URL())
	fmt.Println()
	fmt.Print("Code: ")
	line, err := bufio.NewReader(os.Stdin).ReadString('\n')
	if err != nil && !errors.Is(err, io.EOF) {
		return err
	}
	if strings.TrimSpace(line) == "" {
		return errors.New("no code given; run `steel login` again")
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	if err := login.Finish(ctx, line); err != nil {
		return err
	}
	fmt.Printf("\nLogged in to Claude; tokens saved in %s\n", path)
	fmt.Println("Use them with: steel config set auth claude-oauth")
	return nil
}

func runLogout(args []string) error {
	fs := flag.NewFlagSet("logout", flag.ExitOnError)
	configPath := fs.String("config", defaultConfigPath(), "settings file; tokens are saved in .env beside it")
	fs.Parse(args)
	path := oauthPath(*configPath)
	if err := anthropicauth.RemoveTokens(path); err != nil {
		return err
	}
	fmt.Printf("Logged out of Claude; removed its tokens from %s\n", path)
	return nil
}
