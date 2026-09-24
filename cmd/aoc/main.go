package main

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"
)

// version/commit/buildDate are injected via -ldflags at build time (see
// cmd/aoc/Makefile and dist/build.sh). When built without ldflags (e.g. `go
// run`), they fall back to $PINARD_HOME/BUILD_INFO written by `make dist` or
// `./install` (scripts/build-info.sh).
var (
	version   = "dev"
	commit    = "unknown"
	buildDate = "unknown"
)

var rootCmd = &cobra.Command{
	Use:   "aoc",
	Short: "Pinard agent orchestration CLI",
}

func loadBuildInfoFallback() {
	if version != "dev" {
		return
	}
	home := os.Getenv("PINARD_HOME")
	if home == "" {
		if exe, err := os.Executable(); err == nil {
			home = filepath.Dir(filepath.Dir(exe))
		}
	}
	if home == "" {
		return
	}
	f, err := os.Open(filepath.Join(home, "BUILD_INFO"))
	if err != nil {
		return
	}
	defer f.Close()

	vals := map[string]string{}
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := scanner.Text()
		k, v, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		vals[k] = v
	}
	if v, ok := vals["TAG"]; ok {
		version = v
	}
	if v, ok := vals["COMMIT_SHORT"]; ok {
		commit = v
	}
	if v, ok := vals["BUILT_AT"]; ok {
		buildDate = v
	}
}

func main() {
	loadBuildInfoFallback()
	rootCmd.Version = fmt.Sprintf("%s (%s, built %s)", version, commit, buildDate)
	if err := rootCmd.Execute(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
