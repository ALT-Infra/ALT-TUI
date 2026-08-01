package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"runtime"
	"syscall"

	"altv1/internal/cli"
)

func main() {
	// winit must create its event loop on the process main thread. Detect the
	// private GUI entrypoint before signal setup, Cobra, database work, or
	// provider I/O gives the Go scheduler any reason to migrate this goroutine.
	if hasArgument(os.Args[1:], "__native-gui") {
		runtime.LockOSThread()
		defer runtime.UnlockOSThread()
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := cli.Execute(ctx, os.Args[1:], os.Stdin, os.Stdout, os.Stderr); err != nil {
		fmt.Fprintln(os.Stderr, "alt:", err)
		os.Exit(1)
	}
}

func hasArgument(values []string, expected string) bool {
	for _, value := range values {
		if value == expected {
			return true
		}
	}
	return false
}
