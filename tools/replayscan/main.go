// replayscan walks a chain33 blockchain.db and reports the transactions whose replay can
// disagree with what the chain recorded -- the ones that can make a replayed block's state
// root stop matching the one on chain and stall a node that syncs from scratch.
//
// Modes:
//
//	replayscan -dump <db> <prefix>                        print the first keys under a prefix
//	replayscan addr  <db> <out.jsonl> <body-prefix> [limit] [maxout]
//	replayscan token <db> <out.jsonl> <body-prefix> [limit] [maxout]
//
// # addr: recipient addresses judged differently by different builds
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
//
// # token: token amounts a build's GenesisInit bound rejects
//
// `tokenFinishCreate` calls `account.GenesisInit(owner, token.GetTotal())`, and the total it
// passes is the one the matching preCreate wrote into state. chain33 v1.70.0 made GenesisInit
// run `types.CheckAmount(amount, precision)`, which rejects `amount >= MaxCoin(1e9) *
// coinPrecision(1e8)`, while preCreate allows anything up to MaxTokenBalance (9e18) -- so a
// token whose total sits above that bound can no longer finish. Blocks on chain recorded such
// a finish as a success, so a build carrying the bound diverges at that height. This mode
// lists the finishCreate transactions whose preCreate total falls outside the bound.
package main

import (
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"strconv"
	"strings"

	"github.com/33cn/chain33/common/address"
	"github.com/33cn/chain33/types"

	// Registers the address drivers (btc, btcMultiSign, utxo, eth) via their init.
	_ "github.com/33cn/chain33/system/address/btc"
	_ "github.com/33cn/chain33/system/address/eth"

	"github.com/syndtr/goleveldb/leveldb"
	"github.com/syndtr/goleveldb/leveldb/opt"
	"github.com/syndtr/goleveldb/leveldb/util"
	"google.golang.org/protobuf/encoding/protowire"
	"google.golang.org/protobuf/proto"
)

// maxCoinPrecision is MaxCoin(1e9) * coinPrecision(1e8), the bound types.CheckAmount imposes
// on a token genesis. What #1401 changed is whether GenesisInit runs that check at all.
const maxCoinPrecision = int64(1e17)

// The token oneof arms this tool decodes, and the field numbers inside them. plugin cannot be
// imported from chain33, so both are walked by hand.
//
//	TokenAction        { preCreate = 1, finishCreate = 2 }
//	TokenPreCreate     { name = 1, symbol = 2, introduction = 3, total = 4 }
//	TokenFinishCreate  { symbol = 1, owner = 2 }
const (
	tokenPreCreate    = 1
	tokenFinishCreate = 2
)

// bityuan's driver enable heights. Raising these changes every verdict below that height,
// so they are the heights a candidate has to be compared across.
const (
	btcMultiSignEnable = int64(2270000)
	ethEnable          = int64(19900000)
)

// bodyRowPrefix selects the table rows that hold a BlockBody. The CHAIN-body table also
// stores an index row per block (same height and hash, keyed "-i-hash-..." and holding only
// the primary key), which this prefix skips, so the walk only visits real bodies.
const bodyRowPrefix = "CHAIN-body-body-d-"

// rowHeaderLen is the table package's row header: 8-byte length + 12-byte height + 32-byte hash.
const rowHeaderLen = 52

const keyPrefixLen = len(bodyRowPrefix)

type addrHit struct {
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

type tokenHit struct {
	Height  int64  `json:"height"`
	TxIndex int    `json:"tx_index"`
	Execer  string `json:"execer"`
	Symbol  string `json:"symbol"`
	Total   int64  `json:"total"`
	Receipt int32  `json:"receipt"`
}

// verdictStr renders what address.CheckAddress answered, which is what Exec consults.
func verdictStr(err error) string {
	if err == nil {
		return "ACCEPT"
	}
	return err.Error()
}

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

// tokenActionArm splits a TokenAction into its oneof arm number and the arm's own bytes, so
// the caller can read fields out of preCreate or finishCreate without knowing which it got.
func tokenActionArm(payload []byte) (int, []byte, bool) {
	for len(payload) > 0 {
		num, typ, n := protowire.ConsumeTag(payload)
		if n < 0 {
			return 0, nil, false
		}
		payload = payload[n:]
		if (num == tokenPreCreate || num == tokenFinishCreate) && typ == protowire.BytesType {
			sub, n2 := protowire.ConsumeBytes(payload)
			if n2 < 0 {
				return 0, nil, false
			}
			return int(num), sub, true
		}
		n2 := protowire.ConsumeFieldValue(num, typ, payload)
		if n2 < 0 {
			return 0, nil, false
		}
		payload = payload[n2:]
	}
	return 0, nil, false
}

func protoString(msg []byte, field int) (string, bool) {
	for len(msg) > 0 {
		num, typ, n := protowire.ConsumeTag(msg)
		if n < 0 {
			return "", false
		}
		msg = msg[n:]
		if int(num) == field && typ == protowire.BytesType {
			v, n2 := protowire.ConsumeBytes(msg)
			if n2 < 0 {
				return "", false
			}
			return string(v), true
		}
		n2 := protowire.ConsumeFieldValue(num, typ, msg)
		if n2 < 0 {
			return "", false
		}
		msg = msg[n2:]
	}
	return "", false
}

func protoInt(msg []byte, field int) (int64, bool) {
	for len(msg) > 0 {
		num, typ, n := protowire.ConsumeTag(msg)
		if n < 0 {
			return 0, false
		}
		msg = msg[n:]
		if int(num) == field && typ == protowire.VarintType {
			v, n2 := protowire.ConsumeVarint(msg)
			if n2 < 0 {
				return 0, false
			}
			return int64(v), true
		}
		n2 := protowire.ConsumeFieldValue(num, typ, msg)
		if n2 < 0 {
			return 0, false
		}
		msg = msg[n2:]
	}
	return 0, false
}

// isTokenExecer reports whether the transaction's execer is the one this run was asked for.
// A para chain's token lives at "user.p.<title>.token", and the main chain does not execute
// those -- it records every one of them as ExecPack -- so they are not evidence about the
// main chain's token dapp at all. They were 180 of 200 hits on bityuan, all noise; a run that
// wants a para chain passes its own execer with -execer.
func isTokenExecer(execer, wanted string) bool {
	return execer == wanted
}

// isAddrCandidate decides whether an address is worth reporting, using the fact that the
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
func isAddrCandidate(eStrict, ePermissive error) bool {
	heightDependent := ePermissive == nil && eStrict != nil
	errorDependent := ePermissive != nil && ePermissive != address.ErrAddressLength
	return heightDependent || errorDependent
}

// tokenScan walks a chain, remembering every preCreate total and reporting the finishCreate
// transactions whose total the bounded CheckAmount would reject. Symbols are unique per
// chain and a finishCreate always follows its preCreate, so the map is both small and safe
// to build as the walk advances.
func tokenScan(emit func(interface{}) error, tx *types.Transaction, height int64, txIndex int, receipt int32, wanted string, totals map[string]int64, stats map[string]int64) (bool, error) {
	if !isTokenExecer(string(tx.GetExecer()), wanted) {
		// Keep the para-chain ones visible as a count rather than dropping them silently: a
		// run whose count is surprising is a run whose -execer is wrong.
		if strings.HasSuffix(string(tx.GetExecer()), ".token") {
			stats["__other_chain_token_skipped"]++
		}
		return false, nil
	}
	payload := tx.GetPayload()
	arm, body, ok := tokenActionArm(payload)
	if !ok {
		return false, nil
	}
	switch arm {
	case tokenPreCreate:
		// symbol = 2, total = 4
		if sym, ok := protoString(body, 2); ok {
			if total, ok := protoInt(body, 4); ok {
				totals[sym] = total
			}
		}
	case tokenFinishCreate:
		// symbol = 1
		sym, ok := protoString(body, 1)
		if !ok {
			return false, nil
		}
		total, ok := totals[sym]
		if !ok {
			stats["__finish_without_precreate"]++
			return false, nil
		}
		if total < maxCoinPrecision && total >= 0 {
			return false, nil
		}
		stats["over_bound"]++
		return true, emit(tokenHit{
			Height:  height,
			TxIndex: txIndex,
			Execer:  string(tx.GetExecer()),
			Symbol:  sym,
			Total:   total,
			Receipt: receipt,
		})
	}
	return false, nil
}

// scan walks the block bodies in height order and reports the candidates the mode selects.
func scan(dbPath, outPath, prefix, mode, tokenExecer string, limit, maxOut int64) {
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
	emit := func(v interface{}) error { return enc.Encode(v) }

	iter := db.NewIterator(util.BytesPrefix([]byte(prefix)), nil)
	defer iter.Release()

	var nRows, nTxs, nHits int64
	var lastHeight int64
	stats := map[string]int64{}
	totals := map[string]int64{}

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
			rcpt := int32(-1)
			if i < len(body.Receipts) && body.Receipts[i] != nil {
				rcpt = int32(body.Receipts[i].GetTy())
			}
			pushed := false

			if mode == modeToken {
				var err error
				pushed, err = tokenScan(emit, tx, body.Height, i, rcpt, tokenExecer, totals, stats)
				if err != nil {
					fmt.Fprintln(os.Stderr, "encode:", err)
					os.Exit(1)
				}
			} else {
				to := tx.GetTo()
				if to == "" {
					continue
				}
				// Two heights bracket the verdict: 0 enables the fewest drivers, ethEnable
				// the most, and the set only grows in between. Comparing those two decides
				// whether anything can flip at all.
				eStrict := address.CheckAddress(to, 0)
				ePermissive := address.CheckAddress(to, ethEnable)
				if !isAddrCandidate(eStrict, ePermissive) {
					continue
				}
				h := addrHit{
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
				pushed = true
			}

			if pushed {
				nHits++
			}
		}

		if nRows%100000 == 0 {
			fmt.Fprintf(os.Stderr, "progress rows=%d txs=%d hits=%d height=%d\n", nRows, nTxs, nHits, body.Height)
		}
		if limit > 0 && nRows >= limit {
			fmt.Fprintf(os.Stderr, "stopped at limit=%d\n", limit)
			break
		}
		// Stop before the output can fill the filesystem. A write target with no ceiling is
		// how the first run of this tool filled a 20G root partition.
		if maxOut > 0 && nHits >= maxOut {
			fmt.Fprintf(os.Stderr, "stopped: hits reached maxOut=%d (output capped)\n", maxOut)
			break
		}
	}
	if err := iter.Error(); err != nil {
		fmt.Fprintln(os.Stderr, "iterator:", err)
	}

	fmt.Fprintf(os.Stderr, "done mode=%s rows=%d txs=%d hits=%d lastHeight=%d\n", mode, nRows, nTxs, nHits, lastHeight)
	for k, v := range stats {
		fmt.Fprintf(os.Stderr, "  %-28s %d\n", k, v)
	}
}

const (
	modeAddr  = "addr"
	modeToken = "token"
)

func usage() {
	fmt.Fprintln(os.Stderr, "usage: replayscan -dump <db> <prefix>")
	fmt.Fprintln(os.Stderr, "       replayscan addr  <db> <out.jsonl> <body-prefix> [limit] [maxout]")
	fmt.Fprintln(os.Stderr, "       replayscan token <db> <out.jsonl> <body-prefix> [limit] [maxout] [-execer <name>]")
	fmt.Fprintln(os.Stderr, "  -execer defaults to the main chain's \"token\"; pass user.p.<title>.token to scan a para chain.")
	os.Exit(2)
}

func main() {
	if len(os.Args) >= 4 && os.Args[1] == "-dump" {
		dump(os.Args[2], os.Args[3], 12)
		return
	}
	// -execer is a flag, so pull it out before the positional arguments are read.
	tokenExecer := "token"
	args := []string{}
	for i := 1; i < len(os.Args); i++ {
		if os.Args[i] == "-execer" {
			if i+1 >= len(os.Args) {
				usage()
			}
			tokenExecer = os.Args[i+1]
			i++
			continue
		}
		args = append(args, os.Args[i])
	}
	os.Args = append([]string{os.Args[0]}, args...)

	if len(os.Args) < 5 {
		usage()
	}
	mode := os.Args[1]
	if mode != modeAddr && mode != modeToken {
		usage()
	}
	var limit, maxOut int64
	if len(os.Args) >= 6 {
		v, err := strconv.ParseInt(os.Args[5], 10, 64)
		if err != nil {
			fmt.Fprintln(os.Stderr, "bad limit:", os.Args[5])
			os.Exit(2)
		}
		limit = v
	}
	if len(os.Args) >= 7 {
		v, err := strconv.ParseInt(os.Args[6], 10, 64)
		if err != nil {
			fmt.Fprintln(os.Stderr, "bad maxout:", os.Args[6])
			os.Exit(2)
		}
		maxOut = v
	}
	scan(os.Args[2], os.Args[3], os.Args[4], mode, tokenExecer, limit, maxOut)
}
