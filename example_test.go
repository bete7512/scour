package scour_test

import (
	"context"
	"fmt"
	"log"
	"sync/atomic"

	"github.com/bete7512/scour"
)

func ExampleWalk() {
	var files atomic.Uint64
	stats, err := scour.Walk(context.Background(), "/usr/share", func(e scour.Entry) error {
		if e.Type == scour.TypeFile {
			files.Add(1)
		}
		return nil
	}, scour.WithWorkers(32))
	if err != nil {
		log.Fatal(err)
	}
	fmt.Printf("%d files in %d directories, %d errors, %v\n", files.Load(), stats.Dirs, stats.Errors, stats.Elapsed)
}

func ExampleScan() {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	s := scour.Scan(ctx, "/usr/share", scour.WithStat(scour.StatFull))
	var size int64
	for e := range s.Entries() {
		if e.Type == scour.TypeFile {
			size += e.Info.Size()
		}
	}
	stats, err := s.Wait()
	if err != nil {
		log.Fatal(err)
	}
	fmt.Printf("%d bytes in %d files\n", size, stats.Files)
}
