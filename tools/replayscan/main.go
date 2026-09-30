// chain33scan walks a chain33 blockchain.db and reports every transaction whose
// recipient address is a "flip candidate": an address whose validity verdict differs
// between the chain33 builds under comparison.
//
// Two modes:
//
//	chain33scan -dump <db> <prefix>      print the first keys under a prefix (layout probe)
//	chain33scan <db> <out.jsonl>         scan and emit hits
//
// The filter is one call. For an address that is a valid base58 form of the normal
// version the check returns nil, and for anything shorter than 25 bytes it returns
// ErrAddressLength; both verdicts are identical across the builds, so only what is left
// can flip. Everything else is classified by the error the btc driver reports, which is
// what decides the fork gates:
//
//	ErrCheckVersion      (version byte != 0x00)   -> tolerated by the ForkMultiSignAddress gate
//	ErrAddressChecksum   (len > 25, checksum bad) -> tolerated by the ForkBase58AddressCheck gate
//	ErrCheckChecksum     (len == 25, checksum bad) -> only tolerated by the extra gate that
//	                                                  v1.72.3 added and that we are reverting
//
// Each hit carries block height, transaction index, execer, the address, the error and the
// receipt type the block was produced with (2 = ExecOk, 1 = ExecPack), so the chain's own
// verdict can be put next to the replay verdict.
package main

import (
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"strconv"

	"github.com/33cn/chain33/common/address"
	"github.com/33cn/chain33/types"
	"github.com/syndtr/goleveldb/leveldb"
	"github.com/syndtr/goleveldb/leveldb/opt"
	"github.com/syndtr/goleveldb/leveldb/util"
	"google.golang.org/protobuf/proto"
)

type hit struct {
	Height  int64  `json:"height"`
	TxIndex int    `json:"tx_index"`
	Execer  string `json:"execer"`
	To      string `json:"to"`
	Err     string `json:"err"`
	Receipt int32  `json:"receipt"`
}

// rowHeaderLen is the table package's row header: 8-byte length + 12-byte height + 32-byte hash.
const rowHeaderLen = 52

// keyPrefix is CHAIN-body + the table name, as seen in the dumped keys.
const keyPrefixLen = len("CHAIN-body-body-d-")

// heightFromKey reads the height that is zero-padded to 12 digits inside the key, which is
// more trustworthy than the field inside the value.
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

// dump prints the first keys under a prefix, so the on-disk layout of a table can be read
// off real data instead of derived from the table package.
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
		// The row is wrapped by the table package: an 8-byte little-endian length (44 =
		// 12 + 32), the 12-byte zero-padded height and the 32-byte block hash, then the
		// BlockBody protobuf itself.
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
			e := address.CheckBase58Address(address.NormalVer, to)
			if e == nil || e == address.ErrAddressLength {
				continue
			}
			nHits++
			stats[e.Error()]++

			rcpt := int32(-1)
			if i < len(body.Receipts) && body.Receipts[i] != nil {
				rcpt = int32(body.Receipts[i].GetTy())
			}
			if err := enc.Encode(hit{
				Height:  body.Height,
				TxIndex: i,
				Execer:  string(tx.GetExecer()),
				To:      to,
				Err:     e.Error(),
				Receipt: rcpt,
			}); err != nil {
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
		fmt.Fprintf(os.Stderr, "  %-24s %d\n", k, v)
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
