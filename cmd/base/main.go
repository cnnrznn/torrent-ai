package main

import (
	"crypto/rand"
	"fmt"
	"os"

	"github.com/cnnrznn/torrent-scratch/pkg/metainfo"
	"github.com/cnnrznn/torrent-scratch/pkg/tracker"
)

func main() {
	peerID := genPeerID()

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

	tResp, err := t.Get(tracker.Request{
		PeerID:     peerID,
		Port:       6881,
		InfoHash:   m.InfoHash(),
		Uploaded:   0,
		Downloaded: 0,
		Left:       0,
	})
	if err != nil {
		panic(err)
	}
	fmt.Println(tResp)
}

func genPeerID() string {
	bs := make([]byte, 20)
	rand.Read(bs)

	return string(bs)
}
