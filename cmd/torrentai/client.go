package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/cnnrznn/torrent-ai/pkg/daemon"
)

// clientConfig holds the flags for the add, ls, status and rm commands.
type clientConfig struct {
	daemon      daemonConfig
	json        bool
	dir         string
	peers       []string
	trackers    []string
	deleteFiles bool
}

var clientUsage = map[string]string{
	"add":    "<file.torrent | magnet:?...>",
	"ls":     "",
	"status": "<infohash>",
	"rm":     "<infohash>",
}

// clientMain runs one of the commands that talk to a running daemon.
func clientMain(name string, args []string, out ...io.Writer) int {
	stdout := io.Writer(os.Stdout)
	if len(out) > 0 {
		stdout = out[0]
	}

	cfg := clientConfig{daemon: daemonConfig{stateDir: defaultStateDir()}}
	r := newRegistry(filepath.Base(os.Args[0])+" "+name, clientUsage[name])
	r.group("Daemon connection")
	registerAPIFlags(r, &cfg.daemon)
	r.group("Output")
	r.boolean(&cfg.json, "json", "print JSON instead of text")
	switch name {
	case "add":
		r.group("Add")
		r.str(&cfg.dir, "o", "directory to download into; defaults to the daemon's -o")
		r.list(&cfg.peers, "peer", "peer address (ip:port) to connect to directly; repeatable or comma-separated")
		r.list(&cfg.trackers, "tracker", "extra announce URL; repeatable or comma-separated")
	case "rm":
		r.group("Remove")
		r.boolean(&cfg.deleteFiles, "delete-files", "also delete the torrent's downloaded files")
	}
	r.fs.SetOutput(os.Stderr)
	r.fs.Usage = func() { r.printUsage(os.Stderr) }

	if err := r.parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return exitComplete
		}
		fmt.Fprintf(os.Stderr, "%v\nrun with -h for help\n", err)
		return exitUsage
	}
	want := 1
	if name == "ls" {
		want = 0
	}
	if r.fs.NArg() != want {
		r.printUsage(os.Stderr)
		return exitUsage
	}

	// A TCP address takes precedence over the socket.
	connect := daemon.ClientConfig{Socket: cfg.daemon.socketPath(), Token: cfg.daemon.apiToken}
	if cfg.daemon.apiAddr != "" {
		connect = daemon.ClientConfig{Addr: cfg.daemon.apiAddr, Token: cfg.daemon.apiToken}
	}
	c, err := daemon.NewClient(connect)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return exitUsage
	}

	ctx := context.Background()
	if err := runClient(ctx, c, name, r.fs.Args(), cfg, stdout); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return exitError
	}
	return exitComplete
}

func runClient(ctx context.Context, c *daemon.Client, name string, args []string, cfg clientConfig, out io.Writer) error {
	switch name {
	case "add":
		req := daemon.AddRequest{Peers: cfg.peers, Trackers: cfg.trackers}
		if cfg.dir != "" {
			dir, err := filepath.Abs(cfg.dir)
			if err != nil {
				return err
			}
			req.Dir = dir
		}
		if strings.HasPrefix(args[0], "magnet:") {
			req.Magnet = args[0]
		} else {
			b, err := os.ReadFile(args[0])
			if err != nil {
				return err
			}
			req.Torrent = b
		}
		st, err := c.Add(ctx, req)
		if err != nil {
			return err
		}
		return printStatus(out, cfg.json, st)

	case "ls":
		list, err := c.List(ctx)
		if err != nil {
			return err
		}
		if cfg.json {
			return json.NewEncoder(out).Encode(list)
		}
		w := tabwriter.NewWriter(out, 0, 4, 2, ' ', 0)
		fmt.Fprintln(w, "INFOHASH\tSTATE\tPROGRESS\tPEERS\tNAME")
		for _, st := range list {
			fmt.Fprintf(w, "%s\t%s\t%s\t%d\t%s\n", st.InfoHash, st.State, progress(st), st.Peers, st.Name)
		}
		return w.Flush()

	case "status":
		st, err := c.Status(ctx, args[0])
		if err != nil {
			return err
		}
		return printStatus(out, cfg.json, st)

	case "rm":
		if err := c.Remove(ctx, args[0], cfg.deleteFiles); err != nil {
			return err
		}
		if !cfg.json {
			fmt.Fprintf(out, "removed %s\n", args[0])
		}
		return nil
	}
	return fmt.Errorf("unknown command %q", name)
}

func progress(st daemon.Status) string {
	if st.Pieces == 0 {
		return "-"
	}
	return fmt.Sprintf("%.1f%%", 100*float64(st.Have)/float64(st.Pieces))
}

func printStatus(out io.Writer, asJSON bool, st daemon.Status) error {
	if asJSON {
		return json.NewEncoder(out).Encode(st)
	}
	w := tabwriter.NewWriter(out, 0, 4, 2, ' ', 0)
	rows := [][2]string{
		{"infohash", st.InfoHash},
		{"name", st.Name},
		{"state", string(st.State)},
		{"progress", progress(st)},
		{"size", fmt.Sprint(st.Size)},
		{"left", fmt.Sprint(st.Left)},
		{"downloaded", fmt.Sprint(st.Downloaded)},
		{"uploaded", fmt.Sprint(st.Uploaded)},
		{"peers", fmt.Sprintf("%d connected, %d unchoking us, %d known, %d banned", st.Peers, st.UnchokedUs, st.KnownPeers, st.Banned)},
		{"dir", st.Dir},
		{"added", st.AddedAt.Format(time.RFC3339)},
	}
	if st.Error != "" {
		rows = append(rows, [2]string{"error", st.Error})
	}
	for _, row := range rows {
		fmt.Fprintf(w, "%s:\t%s\n", row[0], row[1])
	}
	return w.Flush()
}
