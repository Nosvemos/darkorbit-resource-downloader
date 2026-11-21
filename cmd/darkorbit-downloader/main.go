// Command darkorbit-downloader mirrors DarkOrbit game resources.
package main

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"os/signal"
	"runtime"
	"syscall"

	"github.com/mattn/go-isatty"
	"github.com/muesli/termenv"

	"github.com/Nosvemos/Darkorbit-Resource-Downloader/internal/app"
)

func main() {
	os.Exit(run())
}

func run() int {
	// Let the Windows console interpret ANSI colour sequences; restore its
	// previous mode on exit. This is a no-op elsewhere and when stdout is
	// not a console.
	if restore, err := termenv.EnableVirtualTerminalProcessing(termenv.NewOutput(os.Stdout)); err == nil {
		defer func() { _ = restore() }()
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	go func() {
		// After the first signal, restore default handling so a second
		// Ctrl+C terminates the process immediately.
		<-ctx.Done()
		stop()
	}()

	args := os.Args[1:]
	code := app.Run(ctx, args, os.Stdout, os.Stderr)

	// Started without arguments from Explorer (double-click), the console
	// window would close before the usage text can be read.
	if len(args) == 0 && runtime.GOOS == "windows" && isatty.IsTerminal(os.Stdin.Fd()) {
		fmt.Fprint(os.Stderr, "\nPress Enter to exit...")
		_, _ = bufio.NewReader(os.Stdin).ReadString('\n')
	}
	return code
}
