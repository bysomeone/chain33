// paratxstat summarises the CHAIN-paratx table: which para chains have transactions on this
// chain, in which block ranges, and how many.
//
// The table is chain33's own index of para transactions (blockchain/blocktable.go): one row per
// (height, title) whose value is a types.HeightPara carrying that block's TxCount for the title.
// Scanning it costs one pass over the key space -- far less than reading the block bodies it
// points at -- so it is the cheap first cut before deciding whether a body scan is worth it.
//
//	usage: paratxstat <datadir>/blockchain.db
package main

import (
	"encoding/binary"
	"fmt"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/33cn/chain33/types"
	"github.com/syndtr/goleveldb/leveldb"
	"github.com/syndtr/goleveldb/leveldb/opt"
	"github.com/syndtr/goleveldb/leveldb/util"
	"google.golang.org/protobuf/proto"
)

// tableKeyPrefix is the data prefix of the paratx table: "CHAIN-paratx" + "paratx" + "d".
const tableKeyPrefix = "CHAIN-paratx-paratx-d-"

// heightDigits is how the primary key encodes the height: %012d, then the raw title bytes.
const heightDigits = 12

type stat struct {
	Title   string
	Blocks  int64
	Txs     int64
	MinH    int64
	MaxH    int64
	Buckets map[int64]int64 // para txs per 1M-height bucket
}

func main() {
	var detailTitles []string
	args := []string{}
	for i := 1; i < len(os.Args); i++ {
		if os.Args[i] == "-detail" {
			if i+1 >= len(os.Args) {
				fmt.Fprintln(os.Stderr, "usage: paratxstat <leveldb-dir> [-detail title1,title2]")
				os.Exit(2)
			}
			for _, t := range strings.Split(os.Args[i+1], ",") {
				if t = strings.TrimSpace(t); t != "" {
					if !strings.HasSuffix(t, ".") {
						t += "."
					}
					detailTitles = append(detailTitles, t)
				}
			}
			i++
			continue
		}
		args = append(args, os.Args[i])
	}
	if len(args) != 1 {
		fmt.Fprintln(os.Stderr, "usage: paratxstat <leveldb-dir> [-detail title1,title2]")
		os.Exit(2)
	}
	dbPath := args[0]
	db, err := leveldb.OpenFile(dbPath, &opt.Options{ReadOnly: true})
	if err != nil {
		fmt.Fprintln(os.Stderr, "open db:", err)
		os.Exit(1)
	}
	defer db.Close()

	start := time.Now()
	byTitle := map[string]*stat{}
	detailSet := map[string]bool{}
	for _, t := range detailTitles {
		detailSet[t] = true
	}
	detail := map[string][]detailRow{}
	detailRows = detail
	var rows, decoded, badKey, badValue int64
	var firstKey, lastKey string

	it := db.NewIterator(util.BytesPrefix([]byte(tableKeyPrefix)), nil)
	defer it.Release()
	for it.Next() {
		rows++
		k := it.Key()
		if firstKey == "" {
			firstKey = string(k)
		}
		lastKey = string(k)
		rest := k[len(tableKeyPrefix):]
		if len(rest) < heightDigits {
			badKey++
			continue
		}
		h, err := strconv.ParseInt(string(rest[:heightDigits]), 10, 64)
		if err != nil {
			badKey++
			continue
		}
		// A table row is the row encoder's frame: an 8-byte little-endian primary-key length,
		// the primary key itself, then the payload (common/db/table DecodeRow). The primary key
		// here is the 12-digit height followed by the title.
		v := it.Value()
		if len(v) <= 8 {
			badValue++
			continue
		}
		primaryLen := int(binary.LittleEndian.Uint64(v[:8]))
		if primaryLen <= 0 || len(v) < 8+primaryLen {
			badValue++
			continue
		}
		var hp types.HeightPara
		if err := proto.Unmarshal(v[8+primaryLen:], &hp); err != nil {
			badValue++
			continue
		}
		decoded++
		title := hp.GetTitle()
		if title == "" {
			title = string(rest[heightDigits:])
		}
		s := byTitle[title]
		if s == nil {
			s = &stat{Title: title, MinH: h, MaxH: h, Buckets: map[int64]int64{}}
			byTitle[title] = s
		}
		s.Blocks++
		s.Txs += int64(hp.GetTxCount())
		if h < s.MinH {
			s.MinH = h
		}
		if h > s.MaxH {
			s.MaxH = h
		}
		s.Buckets[h/1_000_000]++
		if detailSet[string(rest[heightDigits:])] {
			detail[string(rest[heightDigits:])] = append(detail[string(rest[heightDigits:])],
				detailRow{Height: h, Hash: hp.GetHash(), TxCount: hp.GetTxCount()})
		}
	}
	if err := it.Error(); err != nil {
		fmt.Fprintln(os.Stderr, "iterator:", err)
	}

	list := make([]*stat, 0, len(byTitle))
	for _, s := range byTitle {
		list = append(list, s)
	}
	sort.Slice(list, func(i, j int) bool {
		if list[i].Txs != list[j].Txs {
			return list[i].Txs > list[j].Txs
		}
		return list[i].Title < list[j].Title
	})

	fmt.Printf("rows=%d decoded=%d bad_key=%d bad_value=%d titles=%d\n", rows, decoded, badKey, badValue, len(list))
	fmt.Printf("first key %q\nlast key  %q\nscan took %.1fs\n\n", firstKey, lastKey, time.Since(start).Seconds())
	fmt.Printf("%-30s %12s %14s %12s %12s  %s\n", "title", "blocks", "para_txs", "height_min", "height_max", "active 1M-buckets (first..last)")
	for _, s := range list {
		buckets := make([]int64, 0, len(s.Buckets))
		for b := range s.Buckets {
			buckets = append(buckets, b)
		}
		sort.Slice(buckets, func(i, j int) bool { return buckets[i] < buckets[j] })
		fmt.Printf("%-30s %12d %14d %12d %12d  %d buckets %d..%d\n",
			s.Title, s.Blocks, s.Txs, s.MinH, s.MaxH, len(buckets), buckets[0], buckets[len(buckets)-1])
	}

	if len(detailTitles) > 0 {
		detailRows = detail
		stats := scanDetails(db, detail)
		printDetails(detailTitles, stats)
	}
}
