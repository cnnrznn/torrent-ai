package peer

import (
	"github.com/cnnrznn/torrent-scratch/pkg/id"
	"github.com/cnnrznn/torrent-scratch/pkg/metainfo"
)

type Peer struct {
	id   id.Hash
	port int

	torrent metainfo.Metainfo

	// information for tracker
	downloaded int
	uploaded   int
	left       int

	done chan struct{}
}

func New(m metainfo.Metainfo) (*Peer, error) {
	return &Peer{
		id:      id.NewPeerID(),
		torrent: m,
		port:    6881,
	}, nil
}

// Start starts the peer
func (p *Peer) Start() {
	p.done = make(chan struct{})

	// Start tracker client
	// Start downloader job
}

// Stop stops the peer
func (p *Peer) Stop() {
	close(p.done)
}
