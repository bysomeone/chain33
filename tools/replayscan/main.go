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
	"sort"
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

// p2pstore.db keeps the same block bodies under a second key space, "chunk-" + the height
// zero-padded to 12 digits (p2pstore/genChunkDBKey), with the BlockBody stored raw -- no table
// row header. It is worth reading because a shard-enabled node prunes CHAIN-body down to
// roughly the last 10k heights, while this copy is not pruned.
const chunkPrefix = "chunk-"

const (
	srcChainBody = "chainbody" // blockchain.db, CHAIN-body-body-d- rows with a 52-byte header
	srcP2PStore  = "p2pstore"  // p2pstore.db, chunk-<height> values, raw BlockBody
)

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
func heightFromKey(key []byte, off int) (int64, bool) {
	if len(key) < off+12 {
		return 0, false
	}
	h, err := strconv.ParseInt(string(key[off:off+12]), 10, 64)
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

// prefixSizes walks the whole database and groups keys by their leading table name, so "what is
// actually taking the space" comes from the store itself rather than from assuming which tables
// dominate. Values are counted as stored; key bytes are reported separately.
func prefixSizes(dbPath string) {
	db := open(dbPath)
	defer db.Close()

	type stat struct{ keys, kbytes, vbytes int64 }
	sizes := map[string]*stat{}
	var totalKeys, totalBytes int64

	iter := db.NewIterator(nil, nil)
	defer iter.Release()
	for iter.Next() {
		name := leadingTable(iter.Key())
		s := sizes[name]
		if s == nil {
			s = &stat{}
			sizes[name] = s
		}
		s.keys++
		s.kbytes += int64(len(iter.Key()))
		s.vbytes += int64(len(iter.Value()))
		totalKeys++
		totalBytes += int64(len(iter.Key())) + int64(len(iter.Value()))
	}
	if err := iter.Error(); err != nil {
		fmt.Fprintf(os.Stderr, "iteration stopped after %d keys: %v\n", totalKeys, err)
	}

	names := make([]string, 0, len(sizes))
	for n := range sizes {
		names = append(names, n)
	}
	sort.Slice(names, func(i, j int) bool { return sizes[names[i]].vbytes > sizes[names[j]].vbytes })

	fmt.Printf("%-30s %16s %16s %14s\n", "table", "value bytes", "key bytes", "keys")
	for _, n := range names {
		s := sizes[n]
		fmt.Printf("%-30s %16d %16d %14d\n", n, s.vbytes, s.kbytes, s.keys)
	}
	fmt.Printf("%-30s %16d %16d %14d\n", "TOTAL(keys+values)", totalBytes, 0, totalKeys)
	fmt.Fprintf(os.Stderr, "walked %d keys, %d bytes\n", totalKeys, totalBytes)
}

// leadingTable returns the ASCII table name at the start of a key. chain33 puts the table name
// first and then either ASCII digits (heights) or a hash, so stopping at the first byte that is
// neither a letter nor one of "-_." yields the table and nothing else.
func leadingTable(k []byte) string {
	i := 0
	for i < len(k) {
		c := k[i]
		if (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || c == '-' || c == '_' || c == '.' {
			i++
			continue
		}
		break
	}
	if i == 0 {
		return fmt.Sprintf("(binary %x)", head(k, 8))
	}
	return string(k[:i])
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

// protoBytes returns the raw bytes of a length-delimited field.
func protoBytes(msg []byte, field int) ([]byte, bool) {
	for len(msg) > 0 {
		num, typ, n := protowire.ConsumeTag(msg)
		if n < 0 {
			return nil, false
		}
		msg = msg[n:]
		if int(num) == field && typ == protowire.BytesType {
			v, n2 := protowire.ConsumeBytes(msg)
			if n2 < 0 {
				return nil, false
			}
			return v, true
		}
		n2 := protowire.ConsumeFieldValue(num, typ, msg)
		if n2 < 0 {
			return nil, false
		}
		msg = msg[n2:]
	}
	return nil, false
}

// Field numbers from plugin/dapp/paracross/types/paracross.pb.go. ParacrossAction is a oneof
// whose Commit arm is field 1; ParacrossCommitAction carries Status at field 1; and
// ParacrossNodeStatus carries MainBlockHeight at 2, Title at 3 and CrossTxResult at 12.
const (
	paraActionCommit          = 1
	paraActionTy              = 2
	paraCommitStatus          = 1
	paraStatusMainBlockHeight = 2
	paraStatusTitle           = 3
	paraStatusCrossTxResult   = 12
)

// paracrossTy reads ParacrossAction.Ty, which is what decides whether a .paracross transaction
// does any work: execCrossTxNew only acts on the asset-transfer values and returns early for
// everything else. It still has to decode the payload to find that out, which is why the bytes
// of every .paracross transaction are currently read even though most are consensus commits.
func paracrossTy(payload []byte) (int64, bool) { return protoInt(payload, paraActionTy) }

// Cross-chain asset-transfer Ty values, per FilterParaCrossAssetTxHashes. 10002-10004 are
// deliberately excluded by that function's own comment.
const (
	tyAssetTransfer      = 10000
	tyAssetWithdraw      = 10001
	tyCrossAssetTransfer = 10005
)

// isAssetTransferTy reports whether a transaction is one execCrossTxNew would act on.
func isAssetTransferTy(ty int64) bool {
	return ty == tyAssetTransfer || ty == tyAssetWithdraw || ty >= tyCrossAssetTransfer
}

// paraCrossStatusBitMapVerLen mirrors pt.ParaCrossStatusBitMapVerLen. A CrossTxResult of
// exactly this length means "version marker, no cross-chain asset transfers", which is what
// lets getCrossTxsByRst return before reading the referenced main-chain block. Any other
// length is a commit that will read that block.
const paraCrossStatusBitMapVerLen = 4

// paracrossCommit decodes the anchor a paracross Commit transaction points at.
//
// The anchor is the main-chain block the para chain built that block from
// (paracreate.go getNewBlock: MainHeight = mainBlock.Header.Height). A para chain that is
// still catching up on main-chain sequencing therefore commits against an older main height
// than the one its transaction ends up sitting in.
func paracrossCommit(payload []byte) (anchor int64, title string, crossLen int, ok bool) {
	commit, ok := protoBytes(payload, paraActionCommit)
	if !ok {
		return 0, "", 0, false
	}
	status, ok := protoBytes(commit, paraCommitStatus)
	if !ok {
		return 0, "", 0, false
	}
	anchor, ok = protoInt(status, paraStatusMainBlockHeight)
	if !ok {
		return 0, "", 0, false
	}
	title, _ = protoString(status, paraStatusTitle)
	cross, _ := protoBytes(status, paraStatusCrossTxResult)
	return anchor, title, len(cross), true
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

// paraCount folds one transaction into the para-mode histogram and reports whether it was a
// paracross transaction. The match is on substring because a para chain's copy of the
// executor is namespaced (user.p.<title>.paracross) while the main chain's own is "paracross";
// both reach the same code that fetches the referenced main-chain block.
func paraCount(buckets map[int64]map[string]int64, execer string, height int64) bool {
	if !strings.Contains(execer, "paracross") {
		return false
	}
	b := height / paraBucketSize
	if buckets[b] == nil {
		buckets[b] = map[string]int64{}
	}
	buckets[b][execer]++
	return true
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
func scan(dbPath, outPath, prefix, mode, src, tokenExecer string, limit, maxOut int64) {
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

	// A CHAIN-body row carries the table package's 52-byte header; a p2pstore chunk- value is
	// the raw BlockBody.
	headerLen, keyOff := rowHeaderLen, keyPrefixLen
	if src == srcP2PStore {
		headerLen, keyOff = 0, len(chunkPrefix)
	}

	iter := db.NewIterator(util.BytesPrefix([]byte(prefix)), nil)
	defer iter.Release()

	var nRows, nTxs, nHits int64
	var lastHeight int64
	stats := map[string]int64{}
	totals := map[string]int64{}
	paraBuckets := map[int64]map[string]int64{}
	anchors := map[anchorKey]*anchorRow{}
	var sz sizeStat

	for iter.Next() {
		raw := iter.Value()
		if len(raw) < headerLen {
			stats["__too_short"]++
			continue
		}
		var body types.BlockBody
		if err := proto.Unmarshal(raw[headerLen:], &body); err != nil {
			stats["__unmarshal_failed"]++
			continue
		}
		nRows++
		if h, ok := heightFromKey(iter.Key(), keyOff); ok {
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

			switch {
			case mode == modeToken:
				var err error
				pushed, err = tokenScan(emit, tx, body.Height, i, rcpt, tokenExecer, totals, stats)
				if err != nil {
					fmt.Fprintln(os.Stderr, "encode:", err)
					os.Exit(1)
				}
			case mode == modePara:
				// Executing a paracross Commit tx makes the node fetch the main-chain block
				// named by status.MainBlockHash, which on a shard node is a p2pstore round
				// trip. Count those transactions per height band so the cost curve can be
				// compared against an observed sync rate.
				pushed = paraCount(paraBuckets, string(tx.GetExecer()), body.Height)
			case mode == modeAnchor:
				// Same cost, but resolved per commit: which main-chain height it points at,
				// and whether its bitmap says a fetch will happen at all.
				if !strings.Contains(string(tx.GetExecer()), "paracross") {
					continue
				}
				anchor, title, crossLen, ok := paracrossCommit(tx.GetPayload())
				if !ok {
					stats["__anchor_decode_failed"]++
					continue
				}
				k := anchorKey{band: body.Height / paraBucketSize, title: title}
				row := anchors[k]
				if row == nil {
					row = &anchorRow{Band: k.band, HeightFrom: k.band * paraBucketSize,
						HeightTo: k.band*paraBucketSize + paraBucketSize - 1, Title: title}
					anchors[k] = row
				}
				row.Commits++
				if crossLen != paraCrossStatusBitMapVerLen {
					row.WouldFetch++
				}
				if row.AnchorMin == 0 || anchor < row.AnchorMin {
					row.AnchorMin = anchor
				}
				if anchor > row.AnchorMax {
					row.AnchorMax = anchor
				}
				pushed = true
			case mode == modeSize:
				// Nothing per transaction; modeSize is accounted per block, after this loop.
			case mode == modeAddr:
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

		if mode == modeSize {
			// What would a dedicated paracross index cost, against the bodies it replaces?
			// This is block-level: counting it inside the per-transaction loop below would
			// multiply every figure by the block's transaction count.
			sz.Bodies++
			sz.BodyBytes += int64(len(raw))
			sz.AllTxCount += int64(len(body.Txs))
			var inPara bool
			for _, tx := range body.Txs {
				ex := string(tx.GetExecer())
				if !isParaExec(ex) {
					continue
				}
				inPara = true
				// proto.Size walks the fields without allocating; types.Encode marshals into a
				// fresh buffer per transaction, which is what made this mode crawl.
				n := int64(proto.Size(tx))
				sz.ParaTxCount++
				sz.ParaTxBytes += n
				if isCrossExec(ex) {
					sz.CrossTxCount++
					sz.CrossTxBytes += n
					ty, ok := paracrossTy(tx.GetPayload())
					if !ok {
						stats["__ty_decode_failed"]++
					} else {
						if sz.CrossByTyCount == nil {
							sz.CrossByTyCount = map[string]int64{}
							sz.CrossByTyBytes = map[string]int64{}
						}
						k := strconv.FormatInt(ty, 10)
						sz.CrossByTyCount[k]++
						sz.CrossByTyBytes[k] += n
						if isAssetTransferTy(ty) {
							sz.AssetTxCount++
							sz.AssetTxBytes += n
						}
					}
				}
			}
			if inPara {
				sz.ParaBlocks++
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

	if mode == modeSize {
		if err := enc.Encode(sz); err != nil {
			fmt.Fprintln(os.Stderr, "encode:", err)
			os.Exit(1)
		}
	}

	if mode == modeAnchor {
		rows := make([]*anchorRow, 0, len(anchors))
		for _, r := range anchors {
			rows = append(rows, r)
		}
		sort.Slice(rows, func(i, j int) bool {
			if rows[i].Band != rows[j].Band {
				return rows[i].Band < rows[j].Band
			}
			return rows[i].Title < rows[j].Title
		})
		for _, r := range rows {
			if err := enc.Encode(r); err != nil {
				fmt.Fprintln(os.Stderr, "encode:", err)
				os.Exit(1)
			}
		}
	}

	// The histogram is small and bounded by the height range, so it is written once at the
	// end rather than streamed per hit -- output size does not depend on the data.
	if mode == modePara {
		keys := make([]int64, 0, len(paraBuckets))
		for b := range paraBuckets {
			keys = append(keys, b)
		}
		sort.Slice(keys, func(i, j int) bool { return keys[i] < keys[j] })
		for _, b := range keys {
			execers := make([]string, 0, len(paraBuckets[b]))
			for ex := range paraBuckets[b] {
				execers = append(execers, ex)
			}
			sort.Strings(execers)
			for _, ex := range execers {
				if err := enc.Encode(paraBucket{
					Bucket: b,
					From:   b * paraBucketSize,
					To:     b*paraBucketSize + paraBucketSize - 1,
					Execer: ex,
					Count:  paraBuckets[b][ex],
				}); err != nil {
					fmt.Fprintln(os.Stderr, "encode:", err)
					os.Exit(1)
				}
			}
		}
	}
}

const (
	modeAddr   = "addr"
	modeToken  = "token"
	modePara   = "para"
	modeAnchor = "anchor"
	modeSize   = "size"
)

// sizeStat measures how much of the block-body store a paracross index would have to hold.
// It exists to size the proposal in the issue: instead of keeping whole block bodies so that
// paracross can read the few transactions it needs, keep just those transactions.
type sizeStat struct {
	Bodies       int64 `json:"bodies"`
	BodyBytes    int64 `json:"body_bytes"`
	ParaBlocks   int64 `json:"blocks_with_para_txs"`
	ParaTxCount  int64 `json:"para_tx_count"`
	ParaTxBytes  int64 `json:"para_tx_bytes"`
	CrossTxCount int64 `json:"cross_tx_count"`
	CrossTxBytes int64 `json:"cross_tx_bytes"`
	AllTxCount   int64 `json:"all_tx_count"`

	// Breakdown of the .paracross set by ParacrossAction.Ty. Only the asset-transfer values are
	// acted on by execCrossTxNew; the rest still have their payload decoded, but their bytes
	// could be replaced by the Ty alone if the index carried it.
	CrossByTyCount map[string]int64 `json:"cross_count_by_ty"`
	CrossByTyBytes map[string]int64 `json:"cross_bytes_by_ty"`
	AssetTxCount   int64            `json:"asset_tx_count"`
	AssetTxBytes   int64            `json:"asset_tx_bytes"`
}

// isParaExec reports whether an execer names a para chain (user.p.<title>.<execer>).
func isParaExec(execer string) bool { return strings.HasPrefix(execer, "user.p.") }

// isCrossExec reports whether the transaction is one FilterParaCrossTxs would return: a para
// transaction whose execer ends in ".paracross".
func isCrossExec(execer string) bool {
	return isParaExec(execer) && strings.HasSuffix(execer, ".paracross")
}

// anchorKey groups the anchor histogram by the band a commit sits in and its para chain.
type anchorKey struct {
	band  int64
	title string
}

// anchorRow is one row of the anchor histogram: paracross Commit transactions that sit in
// [HeightFrom, HeightTo], grouped by para chain, with the main-chain heights they point at.
type anchorRow struct {
	Band       int64  `json:"band"`
	HeightFrom int64  `json:"height_from"`
	HeightTo   int64  `json:"height_to"`
	Title      string `json:"title"`
	Commits    int64  `json:"commits"`
	WouldFetch int64  `json:"would_fetch"`
	AnchorMin  int64  `json:"anchor_min"`
	AnchorMax  int64  `json:"anchor_max"`
}

// paraBucketSize is the height width of one histogram row in para mode.
const paraBucketSize = int64(100000)

// paraBucket is one row of the para-mode density histogram: how many paracross
// transactions sit in [From, To]. Emitted one JSON line per (bucket, execer).
type paraBucket struct {
	Bucket int64  `json:"bucket"`
	From   int64  `json:"height_from"`
	To     int64  `json:"height_to"`
	Execer string `json:"execer"`
	Count  int64  `json:"count"`
}

func usage() {
	fmt.Fprintln(os.Stderr, "usage: replayscan -dump <db> <prefix>")
	fmt.Fprintln(os.Stderr, "       replayscan addr  <db> <out.jsonl> <body-prefix> [limit] [maxout]")
	fmt.Fprintln(os.Stderr, "       replayscan token <db> <out.jsonl> <body-prefix> [limit] [maxout] [-execer <name>]")
	fmt.Fprintln(os.Stderr, "       replayscan para  <db> <out.jsonl> <body-prefix> [limit] [maxout]")
	fmt.Fprintln(os.Stderr, "       replayscan anchor <db> <out.jsonl> <body-prefix> [limit] [maxout]")
	fmt.Fprintln(os.Stderr, "       replayscan size  <db> <out.jsonl> <body-prefix> [limit] [maxout]")
	fmt.Fprintln(os.Stderr, "  para counts paracross transactions per height bucket (density histogram, not hits).")
	fmt.Fprintln(os.Stderr, "  size measures how much of the body store a paracross index would have to hold.")
	fmt.Fprintln(os.Stderr, "  anchor resolves each paracross Commit to the main-chain height it points at, and")
	fmt.Fprintln(os.Stderr, "       whether its bitmap says that block will be read. Histogram, not per-tx output.")
	fmt.Fprintln(os.Stderr, "  -execer defaults to the main chain's \"token\"; pass user.p.<title>.token to scan a para chain.")
	fmt.Fprintln(os.Stderr, "  -src selects the store: chainbody (blockchain.db, default) or p2pstore.")
	fmt.Fprintln(os.Stderr, "       A shard-enabled node prunes CHAIN-body to ~the last 10k heights, so for older")
	fmt.Fprintln(os.Stderr, "       history point -src p2pstore at p2pstore.db with prefix \"chunk-\".")
	os.Exit(2)
}

func main() {
	if len(os.Args) >= 4 && os.Args[1] == "-dump" {
		dump(os.Args[2], os.Args[3], 12)
		return
	}
	if len(os.Args) >= 3 && os.Args[1] == "prefixes" {
		prefixSizes(os.Args[2])
		return
	}
	// -execer and -src are flags, so pull them out before the positional arguments are read.
	tokenExecer := "token"
	src := srcChainBody
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
		if os.Args[i] == "-src" {
			if i+1 >= len(os.Args) {
				usage()
			}
			src = os.Args[i+1]
			i++
			continue
		}
		args = append(args, os.Args[i])
	}
	os.Args = append([]string{os.Args[0]}, args...)
	if src != srcChainBody && src != srcP2PStore {
		fmt.Fprintln(os.Stderr, "bad -src (want chainbody or p2pstore):", src)
		os.Exit(2)
	}

	if len(os.Args) < 5 {
		usage()
	}
	mode := os.Args[1]
	if mode != modeAddr && mode != modeToken && mode != modePara && mode != modeAnchor && mode != modeSize {
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
	scan(os.Args[2], os.Args[3], os.Args[4], mode, src, tokenExecer, limit, maxOut)
}
