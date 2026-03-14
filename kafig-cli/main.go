package main

import (
	"context"
	"flag"
	"fmt"
	"os"
)

var (
	sess     *session
	jsonMode bool

	maxOpcodes   uint64
	maxCPUTimeUs uint64
)

func main() {
	ctx := context.Background()

	maxOpc := flag.Uint64("max-opcodes", 0, "max opcodes before interrupting execution (0 = disabled)")
	maxCPU := flag.Uint64("max-cpu-ms", 5_000, "max CPU time in milliseconds before interrupting execution (0 = disabled)")
	jsonFlag := flag.Bool("json", false, "output structured JSON (auto-enabled when stdin is not a TTY)")
	flag.Parse()

	maxOpcodes = *maxOpc
	maxCPUTimeUs = *maxCPU * 1000
	jsonMode = *jsonFlag

	// Auto-detect JSON mode when stdin is piped (not a TTY) and no file arg
	if !jsonMode && flag.NArg() == 0 {
		if fi, err := os.Stdin.Stat(); err == nil && fi.Mode()&os.ModeCharDevice == 0 {
			jsonMode = true
		}
	}

	// File execution mode: kafig-cli <file.js>
	if flag.NArg() > 0 {
		runFile(ctx, flag.Arg(0))
		return
	}

	// JSON stdin mode
	if jsonMode {
		runJSON(ctx)
		return
	}

	// Interactive mode
	runInteractive(ctx)
}

func runFile(ctx context.Context, path string) {
	source, err := os.ReadFile(path)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error reading file: %v\n", err)
		os.Exit(1)
	}

	var initErr error
	sess, initErr = newSession(ctx)
	if initErr != nil {
		fmt.Fprintf(os.Stderr, "%v\n", initErr)
		os.Exit(1)
	}
	defer sess.close(context.Background())

	r := sess.exec(ctx, command{Type: "eval", Source: string(source)})
	if jsonMode {
		printJSON(r)
	} else {
		printInteractive(r)
	}
}
