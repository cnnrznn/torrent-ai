package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"net/netip"
	"os"
	"os/signal"
	"strings"
	"time"

	"github.com/cnnrznn/torrent-scratch/pkg/dht"
	"github.com/cnnrznn/torrent-scratch/pkg/id"
	"github.com/cnnrznn/torrent-scratch/pkg/metainfo"
	"github.com/cnnrznn/torrent-scratch/pkg/swarm"
	"github.com/cnnrznn/torrent-scratch/pkg/torrent"
	"github.com/cnnrznn/torrent-scratch/pkg/tracker"
)

// dhtInterval is how often the DHT lookup for the torrent repeats.
const dhtInterval = 5 * time.Minute

type config struct {
	dir  string
	port int
	seed bool
	dht  bool
}

func main() {
	var cfg config
	flag.StringVar(&cfg.dir, "o", ".", "directory to download into")
	flag.IntVar(&cfg.port, "port", 6881, "TCP port for peers, and UDP port for the DHT")
	flag.BoolVar(&cfg.seed, "seed", false, "keep seeding after the download completes")
	flag.BoolVar(&cfg.dht, "dht", true, "find peers through the DHT")
	verbose := flag.Bool("v", false, "verbose logging")
	flag.Usage = func() {
		fmt.Fprintf(os.Stderr, "usage: %s [flags] <file.torrent | magnet:?...>\n", os.Args[0])
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

	if err := run(ctx, log, flag.Arg(0), cfg); err != nil && ctx.Err() == nil {
		log.Error(err.Error())
		os.Exit(1)
	}
}

func run(ctx context.Context, log *slog.Logger, arg string, cfg config) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	peerID := id.NewPeerID()
	sw := swarm.New(50)

	// A .torrent gives us the metainfo up front; a magnet gives only
	// the infohash and maybe some trackers.
	var m *metainfo.Metainfo
	var mag *metainfo.Magnet
	var infoHash id.Hash
	var err error
	if strings.HasPrefix(arg, "magnet:") {
		if mag, err = metainfo.ParseMagnet(arg); err != nil {
			return err
		}
		infoHash = mag.InfoHash
	} else {
		if m, err = fromFile(arg); err != nil {
			return err
		}
		infoHash = m.InfoHash()
	}

	var node *dht.Server
	if cfg.dht {
		node, err = dht.New(dht.Config{Addr: fmt.Sprintf(":%d", cfg.port), Logger: log})
		if err != nil {
			return err
		}
		go node.Run(ctx)
		go dhtPeers(ctx, log, node, sw, infoHash, cfg.port)
	}

	if mag != nil {
		log.Info("fetching metadata", "name", mag.Name, "infohash", infoHash.Hex(), "trackers", len(mag.Trackers))
		for _, tr := range mag.Trackers {
			go announce(ctx, log, sw, tr, tracker.Request{
				InfoHash: infoHash,
				PeerID:   peerID,
				Port:     cfg.port,
				// The size is unknown until we have the metadata; any
				// non-zero value marks us as a leecher.
				Left:  1,
				Event: tracker.EventStarted,
			})
		}
		if m, err = torrent.FetchMetadata(ctx, sw, infoHash, peerID, log); err != nil {
			return err
		}
	}
	log.Info("loaded torrent", "name", m.Name(), "size", m.Length(), "pieces", m.NumPieces(), "infohash", infoHash.Hex())

	ln, err := net.Listen("tcp", fmt.Sprintf(":%d", cfg.port))
	if err != nil {
		return err
	}

	sessCfg := torrent.Config{
		Dir:      cfg.dir,
		PeerID:   peerID,
		Listener: ln,
		Logger:   log,
	}
	if node != nil {
		sessCfg.DHTPort = cfg.port
		sessCfg.OnPort = node.AddNode
	}
	s, err := torrent.New(m, sw, sessCfg)
	if err != nil {
		ln.Close()
		return err
	}

	if m.Announce() != "" {
		go announce(ctx, log, sw, m.Announce(), tracker.Request{
			InfoHash: infoHash,
			PeerID:   peerID,
			Port:     cfg.port,
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
			if !cfg.seed {
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

func fromFile(path string) (*metainfo.Metainfo, error) {
	bs, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return metainfo.New(string(bs))
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

// dhtPeers joins the DHT and repeatedly looks up and announces the
// torrent, feeding the peers it finds into the swarm.
func dhtPeers(ctx context.Context, log *slog.Logger, node *dht.Server, sw *swarm.Swarm, infoHash id.Hash, port int) {
	for {
		if node.Nodes() == 0 {
			if err := node.Bootstrap(ctx, dht.DefaultRouters); err != nil {
				log.Warn("dht bootstrap failed", "err", err)
			} else {
				log.Info("joined dht", "nodes", node.Nodes())
			}
		}

		found := 0
		err := node.Announce(ctx, infoHash, port, func(peers []netip.AddrPort) {
			found += len(peers)
			sw.Add(peers...)
		})
		if err != nil && ctx.Err() == nil {
			log.Warn("dht lookup failed", "err", err)
		} else {
			log.Info("dht lookup done", "peers", found, "nodes", node.Nodes())
		}

		select {
		case <-ctx.Done():
			return
		case <-time.After(dhtInterval):
		}
	}
}
