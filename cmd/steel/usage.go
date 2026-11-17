package main

import (
	"flag"
	"fmt"
	"io"
	"path/filepath"
	"text/tabwriter"
	"time"

	"github.com/spiiritual/steel/internal/usage"
)

// usagePath is where steel records each model call's tokens: beside the
// config, so a test config elsewhere keeps its own numbers.
func usagePath(configPath string) string {
	return filepath.Join(filepath.Dir(configPath), "usage.jsonl")
}

func runUsage(args []string, out io.Writer) error {
	fs := flag.NewFlagSet("usage", flag.ExitOnError)
	configPath := fs.String("config", defaultConfigPath(), "settings file; usage.jsonl sits beside it")
	since := fs.Duration("since", 24*time.Hour, "how far back to count; 0 counts everything")
	fs.Usage = func() {
		fmt.Fprintln(fs.Output(), "usage: steel usage [-since 24h]\n\nShows tokens and prompt-cache hit rates of model calls, as the provider reported them.")
		fs.PrintDefaults()
	}
	fs.Parse(args)

	var from time.Time
	if *since > 0 {
		from = time.Now().Add(-*since)
	}
	rs, err := usage.Read(usagePath(*configPath), from)
	if err != nil {
		return err
	}
	if len(rs) == 0 {
		fmt.Fprintln(out, "No model calls recorded in that time. Providers that don't report usage leave nothing to count.")
		return nil
	}
	groups, all := usage.Summarize(rs)
	w := tabwriter.NewWriter(out, 0, 0, 2, ' ', tabwriter.AlignRight)
	fmt.Fprintln(w, "purpose\tmodel\tcalls\tinput\tcached\toutput\thit rate\t")
	row := func(t usage.Total) {
		fmt.Fprintf(w, "%s\t%s\t%d\t%d\t%d\t%d\t%.1f%%\t\n", t.Purpose, t.Model, t.Calls, t.Input, t.Cached, t.Output, 100*t.HitRate())
	}
	for _, g := range groups {
		row(g)
	}
	all.Purpose = "all"
	row(all)
	return w.Flush()
}
