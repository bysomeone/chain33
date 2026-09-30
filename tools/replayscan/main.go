// chain33scan walks a chain33 blockchain.db and reports every transaction whose recipient
// address is judged differently at different heights, or by different builds -- the
// addresses that can make a replayed block's state root stop matching the one on chain and
// stall a node that syncs from scratch.
//
// Modes:
//
//	chain33scan -dump <db> <prefix>              print the first keys under a prefix
//	chain33scan <db> <out.jsonl> <body-prefix> [limit]
//
// # The verdict that matters
//
// What a replay actually consults is `address.CheckAddress(addr, height)`, which walks every
// enabled driver and accepts if any one of them does. Two consequences shape this tool:
//
//   - Reading a single driver (e.g. CheckBase58Address with the normal version) is not the
//     verdict. A multisig address (version 0x01) is rejected by `btc` but accepted by
//     `btcMultiSign` once that driver is enabled, so a per-driver reading calls it a flip
//     when it is not.
//   - A driver is enabled from its enableHeight upwards and never switched off, so the set
//     only grows with height and a stricter set can only reject more. Comparing the two
//     extreme heights (0, and the highest enable height) therefore brackets every verdict a
//     replay can produce, and decides whether an address is worth reporting at all.
//
// The per-height verdicts are reported for the heights that matter on bityuan, where the
// driver enable heights are eth = 19900000 and btcMultiSign = 2270000 (btc.go / bityuan.go).
package main

import (
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"strconv"

	"github.com/33cn/chain33/common/address"
	"github.com/33cn/chain33/types"

	// Registers the address drivers (btc, btcMultiSign, utxo, eth) via their init.
	_ "github.com/33cn/chain33/system/address/btc"
	_ "github.com/33cn/chain33/system/address/eth"

	"github.com/syndtr/goleveldb/leveldb"
	"github.com/syndtr/goleveldb/leveldb/opt"
	"github.com/syndtr/goleveldb/leveldb/util"
	"google.golang.org/protobuf/proto"
)

// bityuan's driver enable heights. Raising these changes every verdict below that height,
// so they are the heights a candidate has to be compared across.
const (
	btcMultiSignEnable = int64(2270000)
	ethEnable          = int64(19900000)
)

type hit struct {
	Height  int64  `json:"height"`
	TxIndex int    `json:"tx_index"`
	Execer  string `json:"execer"`
	To      string `json:"to"`
	Receipt int32  `json:"receipt"`

	// Verdicts along the real path (all enabled drivers), at the heights that matter.
	VerdictHere string `json:"verdict_here"` // at the height the transaction sits at
	Verdict0    string `json:"verdict_h0"`   // h=0, the strictest enabled set
	VerdictFork string `json:"verdict_fork"` // h=btcMultiSign enable
	VerdictEth  string `json:"verdict_eth"`  // h=eth enable
}

// verdictStr renders what address.CheckAddress answered, which is what Exec consults.
func verdictStr(err error) string {
	if err == nil {
		return "ACCEPT"
	}
	return err.Error()
}

// isCandidate decides whether an address is worth reporting, using the fact that the
// enabled driver set only grows with height. Two kinds can flip, and both are kept:
//
//   - height-dependent: accepted at the most permissive height but rejected at the
//     strictest one. An eth address is the shape -- rejected while only btc and utxo are
//     enabled, accepted once eth's enable height is reached.
//   - error-dependent: rejected everywhere, but by an error a fork gate or a build change
//     decides on. Block 101641's address is the shape: ErrCheckChecksum at every height,
//     tolerated only by the extra gate v1.72.3 added.
//
// Everything else is noise: an address rejected at every height by something no gate looks
// at -- ErrAddressLength for a string that is not an address at all -- cannot flip, and
// those made up 264 of 272 hits before this filter existed.
func isCandidate(eStrict, ePermissive error) bool {
	heightDependent := ePermissive == nil && eStrict != nil
	errorDependent := ePermissive != nil && ePermissive != address.ErrAddressLength
	return heightDependent || errorDependent
}

// bodyRowPrefix selects the table rows that hold a BlockBody. The CHAIN-body table also
// stores an index row per block (same height and hash, keyed "-i-hash-..." and holding only
// the primary key), which this prefix skips, so the walk only visits real bodies.
const bodyRowPrefix = "CHAIN-body-body-d-"

// rowHeaderLen is the table package's row header: 8-byte length + 12-byte height + 32-byte hash.
const rowHeaderLen = 52

const keyPrefixLen = len(bodyRowPrefix)

// heightFromKey reads the height zero-padded to 12 digits inside the key, which is more
// trustworthy than the field inside the value.
func heightFromKey(key []byte) (int64, bool) {
	if len(key) < keyPrefixLen+12 {
		return 0, false
	}
	h, err := strconv.ParseInt(string(key[keyPrefixLen:keyPrefixLen+12]), 10, 64)
	if err != nil {
		return 0, false
	}
	return h, true
}

func open(dbPath string) *leveldb.DB {
	db, err := leveldb.OpenFile(dbPath, &opt.Options{ReadOnly: true})
	if err != nil {
		fmt.Fprintln(os.Stderr, "open db:", err)
		os.Exit(1)
	}
	return db
}

func dump(dbPath, prefix string, n int) {
	db := open(dbPath)
	defer db.Close()

	iter := db.NewIterator(util.BytesPrefix([]byte(prefix)), nil)
	defer iter.Release()

	count := 0
	for iter.Next() {
		fmt.Printf("key=%s hex=%s vlen=%d vhead=%x\n",
			iter.Key(), hex.EncodeToString(iter.Key()), len(iter.Value()), head(iter.Value(), 64))
		count++
		if count >= n {
			break
		}
	}
	fmt.Fprintf(os.Stderr, "dumped %d keys under prefix %q\n", count, prefix)
}

func head(b []byte, n int) []byte {
	if len(b) < n {
		return b
	}
	return b[:n]
}

func scan(dbPath, outPath, prefix string, limit int64) {
	// Pin the enabled set to bityuan's configuration; the registered default is 0 for every
	// driver, which would accept addresses this chain rejects at low heights.
	address.Init(&address.Config{
		EnableHeight: map[string]int64{
			"btc":          0,
			"btcMultiSign": btcMultiSignEnable,
			"eth":          ethEnable,
		},
		DefaultDriver: "btc",
	})

	db := open(dbPath)
	defer db.Close()

	out, err := os.Create(outPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, "create out:", err)
		os.Exit(1)
	}
	defer out.Close()
	enc := json.NewEncoder(out)

	iter := db.NewIterator(util.BytesPrefix([]byte(prefix)), nil)
	defer iter.Release()

	var nRows, nTxs, nHits int64
	var lastHeight int64
	stats := map[string]int64{}

	for iter.Next() {
		raw := iter.Value()
		if len(raw) < rowHeaderLen {
			stats["__too_short"]++
			continue
		}
		var body types.BlockBody
		if err := proto.Unmarshal(raw[rowHeaderLen:], &body); err != nil {
			stats["__unmarshal_failed"]++
			continue
		}
		nRows++
		if h, ok := heightFromKey(iter.Key()); ok {
			body.Height = h
		}
		lastHeight = body.Height

		for i, tx := range body.Txs {
			nTxs++
			to := tx.GetTo()
			if to == "" {
				continue
			}
			// Two heights bracket the verdict: 0 enables the fewest drivers, ethEnable the
			// most, and the set only grows in between. Comparing those two decides whether
			// anything can flip at all.
			eStrict := address.CheckAddress(to, 0)
			ePermissive := address.CheckAddress(to, ethEnable)
			if !isCandidate(eStrict, ePermissive) {
				continue
			}
			nHits++

			rcpt := int32(-1)
			if i < len(body.Receipts) && body.Receipts[i] != nil {
				rcpt = int32(body.Receipts[i].GetTy())
			}
			h := hit{
				Height:      body.Height,
				TxIndex:     i,
				Execer:      string(tx.GetExecer()),
				To:          to,
				Receipt:     rcpt,
				VerdictHere: verdictStr(address.CheckAddress(to, body.Height)),
				Verdict0:    verdictStr(eStrict),
				VerdictFork: verdictStr(address.CheckAddress(to, btcMultiSignEnable)),
				VerdictEth:  verdictStr(ePermissive),
			}
			stats[h.VerdictHere]++

			if err := enc.Encode(h); err != nil {
				fmt.Fprintln(os.Stderr, "encode:", err)
				os.Exit(1)
			}
		}

		if nRows%100000 == 0 {
			fmt.Fprintf(os.Stderr, "progress rows=%d txs=%d hits=%d height=%d\n", nRows, nTxs, nHits, body.Height)
		}
		if limit > 0 && nRows >= limit {
			fmt.Fprintf(os.Stderr, "stopped at limit=%d\n", limit)
			break
		}
	}
	if err := iter.Error(); err != nil {
		fmt.Fprintln(os.Stderr, "iterator:", err)
	}

	fmt.Fprintf(os.Stderr, "done rows=%d txs=%d hits=%d lastHeight=%d\n", nRows, nTxs, nHits, lastHeight)
	for k, v := range stats {
		fmt.Fprintf(os.Stderr, "  %-28s %d\n", k, v)
	}
}

func main() {
	if len(os.Args) >= 4 && os.Args[1] == "-dump" {
		dump(os.Args[2], os.Args[3], 12)
		return
	}
	if len(os.Args) < 4 {
		fmt.Fprintln(os.Stderr, "usage: chain33scan <db> <out.jsonl> <body-prefix> [limit]")
		fmt.Fprintln(os.Stderr, "       chain33scan -dump <db> <prefix>")
		os.Exit(2)
	}
	var limit int64
	if len(os.Args) >= 5 {
		v, err := strconv.ParseInt(os.Args[4], 10, 64)
		if err != nil {
			fmt.Fprintln(os.Stderr, "bad limit:", os.Args[4])
			os.Exit(2)
		}
		limit = v
	}
	scan(os.Args[1], os.Args[2], os.Args[3], limit)
}
