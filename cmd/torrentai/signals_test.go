package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// buildBinary compiles torrentai into a temp dir.
func buildBinary(t *testing.T) string {
	t.Helper()
	bin := filepath.Join(t.TempDir(), "torrentai")
	out, err := exec.Command("go", "build", "-o", bin, ".").CombinedOutput()
	require.NoError(t, err, string(out))
	return bin
}

func TestStopSignals(t *testing.T) {
	if testing.Short() {
		t.Skip("builds and runs the binary")
	}
	bin := buildBinary(t)

	for _, tc := range []struct {
		sig  syscall.Signal
		name string
		code int
	}{
		{syscall.SIGTERM, "SIGTERM", 143},
		{syscall.SIGINT, "SIGINT", 130},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			cpu, heap := filepath.Join(dir, "cpu.pprof"), filepath.Join(dir, "heap.pprof")

			// No peer sources, so the run waits for metadata until
			// it's signalled.
			cmd := exec.Command(bin,
				"-port", "0", "-dht=false", "-trackers=false", "-log-format", "json",
				"-flame-cpu-file", cpu, "-flame-heap-file", heap,
				"-o", dir,
				"magnet:?xt=urn:btih:7acf8fb590b2060dd9c3146ef770169d593433b0",
			)
			cmd.Env = append(os.Environ(),
				"TORRENT_AI_OTEL=",
				"TORRENT_AI_OTEL_TRACES_EXPORTER=console",
				"TORRENT_AI_OTEL_METRICS_EXPORTER=none",
				"TORRENT_AI_OTEL_LOGS_EXPORTER=none",
			)
			var stdout bytes.Buffer
			cmd.Stdout = &stdout
			stderr, err := cmd.StderrPipe()
			require.NoError(t, err)
			require.NoError(t, cmd.Start())

			// Wait until it's fetching metadata, then signal it.
			lines := make(chan map[string]any, 100)
			go func() {
				sc := bufio.NewScanner(stderr)
				for sc.Scan() {
					var rec map[string]any
					if json.Unmarshal(sc.Bytes(), &rec) == nil {
						lines <- rec
					}
				}
				close(lines)
			}()
			waitFor(t, lines, "fetching metadata")
			require.NoError(t, cmd.Process.Signal(tc.sig))

			exit := waitFor(t, lines, "exit")
			for range lines {
			}
			err = cmd.Wait()
			var exitErr *exec.ExitError
			require.True(t, errors.As(err, &exitErr), "exits non-zero: %v", err)
			require.Equal(t, tc.code, exitErr.ExitCode())

			require.Equal(t, "interrupted", exit["result"])
			require.Equal(t, tc.name, exit["signal"])
			require.EqualValues(t, tc.code, exit["code"])

			// Profiles are written and telemetry flushed on the way out.
			for _, f := range []string{cpu, heap} {
				st, err := os.Stat(f)
				require.NoError(t, err)
				require.Positive(t, st.Size(), f)
			}
			require.Regexp(t, `"Name":\s*"torrentai.run"`, stdout.String(), "root span flushed")
		})
	}
}

// waitFor returns the first log record with the given msg.
func waitFor(t *testing.T, lines <-chan map[string]any, msg string) map[string]any {
	t.Helper()
	timeout := time.After(15 * time.Second)
	for {
		select {
		case rec, ok := <-lines:
			if !ok {
				t.Fatalf("output ended before %q", msg)
			}
			if rec["msg"] == msg {
				return rec
			}
		case <-timeout:
			t.Fatalf("no %q within 15s", msg)
		}
	}
}
