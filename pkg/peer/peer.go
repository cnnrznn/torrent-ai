package peer

import (
	"crypto/rand"

	"github.com/cnnrznn/torrent-scratch/pkg/metainfo"
)

type Peer struct {
	id   string
	port int

	torrent metainfo.Metainfo

	// information for tracker
	downloaded int
	uploaded   int
	left       int

	done chan struct{}
}

func genPeerID() string {
	bs := make([]byte, 20)
	rand.Read(bs)

	return string(bs)
}

func New(m metainfo.Metainfo) (*Peer, error) {
	return &Peer{
		id:      genPeerID(),
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
