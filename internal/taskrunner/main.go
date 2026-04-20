package taskrunner

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
)

// Main is the zero-custom-flag sugar: BindFlags + Parse + signal ctx +
// Run + os.Exit. Exit codes:
//
//	2 — setup failure (wraps ErrSetup: DB open, schema inference, factory probe)
//	1 — any other Run error, including ErrFailedRemain
//	0 — clean drain
//
// Domain binaries that need custom flags should call BindFlags + Run
// directly and take over exit-code policy.
func Main(cfg Config) {
	prog := filepath.Base(os.Args[0])
	fs := flag.NewFlagSet(prog, flag.ExitOnError)
	opts := BindFlags(fs)
	_ = fs.Parse(os.Args[1:])

	ctx, cancel := signal.NotifyContext(context.Background(),
		syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	if _, err := Run(ctx, cfg, *opts); err != nil {
		fmt.Fprintf(os.Stderr, "%s: %v\n", prog, err)
		if errors.Is(err, ErrSetup) {
			os.Exit(2)
		}
		os.Exit(1)
	}
}
