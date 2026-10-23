package main

import (
	"context"
	"fmt"
	"os"

	"github.com/cnnrznn/torrent-scratch/pkg/id"
	"github.com/cnnrznn/torrent-scratch/pkg/metainfo"
	"github.com/cnnrznn/torrent-scratch/pkg/tracker"
)

func main() {
	peerID := id.NewPeerID()

	bs, err := os.ReadFile(os.Args[1])
	if err != nil {
		panic(err)
	}

	m := metainfo.New(string(bs))
	fmt.Println(m)

	t, err := tracker.New(m.Announce())
	if err != nil {
		panic(err)
	}

	tResp, err := t.Announce(context.Background(), tracker.Request{
		PeerID:     peerID,
		Port:       6881,
		InfoHash:   m.InfoHash(),
		Uploaded:   0,
		Downloaded: 0,
		Left:       m.Length(),
		Event:      tracker.EventStarted,
	})
	if err != nil {
		panic(err)
	}
	fmt.Println(tResp)
}
