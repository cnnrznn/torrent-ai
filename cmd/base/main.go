package main

import (
	"fmt"
	"os"

	"github.com/cnnrznn/torrent-scratch/pkg/metainfo"
)

func main() {
	bs, err := os.ReadFile(os.Args[1])
	if err != nil {
		panic(err)
	}

	m := metainfo.New(string(bs))
	fmt.Println(m)
}
