// k6-runner wraps the k6 binary to produce a TestResult JSON after each run.
// It has two modes:
//
//	--install  copies itself to /runner-bin/k6-runner and exits (used as an
//	           init container to stage the binary into a shared volume)
//	default    runs k6 against the script and writes /results/output.json
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"time"

	"github.com/loks0n/synthetics-operator/internal/results"
)

const (
	outputFile = "/results/output.json"
	runnerDest = "/runner-bin/k6-runner"
)

func main() {
	var install bool
	var name, namespace, scriptPath string
	flag.BoolVar(&install, "install", false, "copy self to "+runnerDest+" and exit")
	flag.StringVar(&name, "name", "", "K6Test resource name")
	flag.StringVar(&namespace, "namespace", "", "K6Test resource namespace")
	flag.StringVar(&scriptPath, "script", "/scripts/test.js", "path to k6 script")
	flag.Parse()

	if install {
		if err := installSelf(); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		return
	}

	if err := run(name, namespace, scriptPath); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run(name, namespace, scriptPath string) error {
	start := time.Now()
	cmd := exec.CommandContext(context.Background(), "k6", "run", scriptPath) //nolint:gosec
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	err := cmd.Run()
	durationMs := time.Since(start).Milliseconds()

	result := results.TestResult{
		Kind:       results.KindK6Test,
		Name:       name,
		Namespace:  namespace,
		Success:    err == nil,
		Timestamp:  start,
		DurationMs: durationMs,
	}

	data, jsonErr := json.Marshal(result)
	if jsonErr != nil {
		return fmt.Errorf("marshalling result: %w", jsonErr)
	}
	if writeErr := os.WriteFile(outputFile, data, 0o600); writeErr != nil {
		return fmt.Errorf("writing result: %w", writeErr)
	}

	if err != nil {
		return fmt.Errorf("k6 exited: %w", err)
	}
	return nil
}

func installSelf() error {
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	src, err := os.Open(exe)
	if err != nil {
		return err
	}
	defer src.Close()

	dst, err := os.OpenFile(runnerDest, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o755)
	if err != nil {
		return err
	}
	defer dst.Close()

	_, err = io.Copy(dst, src)
	return err
}
