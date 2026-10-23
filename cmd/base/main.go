package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"os"
	"os/signal"
	"time"

	"github.com/cnnrznn/torrent-scratch/pkg/id"
	"github.com/cnnrznn/torrent-scratch/pkg/metainfo"
	"github.com/cnnrznn/torrent-scratch/pkg/swarm"
	"github.com/cnnrznn/torrent-scratch/pkg/torrent"
	"github.com/cnnrznn/torrent-scratch/pkg/tracker"
)

func main() {
	var (
		dir     = flag.String("o", ".", "directory to download into")
		port    = flag.Int("port", 6881, "port to listen on for peers")
		seed    = flag.Bool("seed", false, "keep seeding after the download completes")
		verbose = flag.Bool("v", false, "verbose logging")
	)
	flag.Usage = func() {
		fmt.Fprintf(os.Stderr, "usage: %s [flags] <file.torrent>\n", os.Args[0])
		flag.PrintDefaults()
	}
	flag.Parse()
	if flag.NArg() != 1 {
		flag.Usage()
		os.Exit(2)
	}

	level := slog.LevelInfo
	if *verbose {
		level = slog.LevelDebug
	}
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: level}))

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

	if err := run(ctx, log, flag.Arg(0), *dir, *port, *seed); err != nil && ctx.Err() == nil {
		log.Error(err.Error())
		os.Exit(1)
	}
}

func run(ctx context.Context, log *slog.Logger, path, dir string, port int, seed bool) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	peerID := id.NewPeerID()

	bs, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	m, err := metainfo.New(string(bs))
	if err != nil {
		return err
	}
	log.Info("loaded torrent", "name", m.Name(), "size", m.Length(), "pieces", m.NumPieces(), "infohash", m.InfoHash().Hex())

	ln, err := net.Listen("tcp", fmt.Sprintf(":%d", port))
	if err != nil {
		return err
	}

	sw := swarm.New(50)

	s, err := torrent.New(m, sw, torrent.Config{
		Dir:      dir,
		PeerID:   peerID,
		Listener: ln,
		Logger:   log,
	})
	if err != nil {
		ln.Close()
		return err
	}

	if m.Announce() != "" {
		go announce(ctx, log, sw, m.Announce(), tracker.Request{
			InfoHash: m.InfoHash(),
			PeerID:   peerID,
			Port:     port,
			Left:     int(s.Stats().Left),
			Event:    tracker.EventStarted,
		})
	}

	errc := make(chan error, 1)
	go func() { errc <- s.Run(ctx) }()

	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()

	complete := s.Complete()
	for {
		select {
		case err := <-errc:
			return err
		case <-complete:
			complete = nil
			if !seed {
				cancel()
				<-errc
				return nil
			}
			log.Info("seeding; press ctrl-c to stop")
		case <-ticker.C:
			st := s.Stats()
			log.Info("progress",
				"pieces", fmt.Sprintf("%d/%d", st.Have, st.Pieces),
				"percent", fmt.Sprintf("%.1f", 100*float64(st.Have)/float64(st.Pieces)),
				"peers", st.Peers,
				"known", sw.Len(),
				"down", st.Downloaded,
				"up", st.Uploaded,
			)
		}
	}
}

func announce(ctx context.Context, log *slog.Logger, sw *swarm.Swarm, url string, req tracker.Request) {
	t, err := tracker.New(url)
	if err != nil {
		log.Warn("bad tracker", "url", url, "err", err)
		return
	}
	res, err := t.Announce(ctx, req)
	if err != nil {
		log.Warn("announce failed", "url", url, "err", err)
		return
	}
	log.Info("tracker returned peers", "url", url, "peers", len(res.Peers))
	sw.Add(res.Peers...)
}
