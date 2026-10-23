package torrent

import (
	"context"
	"log/slog"
	"net/netip"
	"time"

	"github.com/cnnrznn/torrent-scratch/pkg/id"
	"github.com/cnnrznn/torrent-scratch/pkg/metainfo"
	"github.com/cnnrznn/torrent-scratch/pkg/peerwire"
	"github.com/cnnrznn/torrent-scratch/pkg/swarm"
)

const (
	metadataAttempts = 8
	metadataTimeout  = 30 * time.Second
)

// FetchMetadata gets the info dict for a magnet link from peers in the
// swarm, trying several at once. It waits for peer sources to add
// addresses until one succeeds or ctx is cancelled.
func FetchMetadata(ctx context.Context, sw *swarm.Swarm, infoHash, peerID id.Hash, log *slog.Logger) (*metainfo.Metainfo, error) {
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}

	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	results := make(chan string, 1)
	slots := make(chan struct{}, metadataAttempts)

	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()

	for {
		for len(slots) < cap(slots) {
			addr, ok := sw.Next()
			if !ok {
				break
			}
			slots <- struct{}{}
			go func() {
				defer func() { <-slots }()
				raw, err := fetchMetadataFrom(ctx, addr, infoHash, peerID)
				sw.Release(addr, err != nil)
				if err != nil {
					log.Debug("metadata fetch failed", "addr", addr, "err", err)
					return
				}
				select {
				case results <- raw:
				default:
				}
			}()
		}

		select {
		case raw := <-results:
			return metainfo.FromInfoDict(raw)
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-sw.Notify():
		case <-ticker.C:
		}
	}
}

func fetchMetadataFrom(ctx context.Context, addr netip.AddrPort, infoHash, peerID id.Hash) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, metadataTimeout)
	defer cancel()

	c, err := peerwire.Dial(ctx, addr, peerwire.NewHandshake(infoHash, peerID, false))
	if err != nil {
		return "", err
	}
	defer c.Close()

	return peerwire.FetchMetadata(ctx, c, infoHash)
}
