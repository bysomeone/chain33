package main

import (
	"encoding/hex"
	"encoding/json"

	"github.com/33cn/chain33/types"
	"google.golang.org/protobuf/encoding/protowire"
)

// Rollup-mode evidence lives in two places on the main chain, and this mode collects both:
//
//   - the rollup dapp's own transactions (execer "rollup"), one per commit round, each carrying
//     a CheckPoint with the chain title, the round number and the para-chain transactions the
//     round covers (plugin/dapp/rollup/proto/rollup.proto);
//   - the paracross RollupCrossTx transactions (execer "paracross", ParacrossAction arm 15) that
//     settle the cross-chain asset transfers of a round (plugin/dapp/paracross/executor/rollupcrosstx.go).
//
// Either one existing means a para chain is running the rollup mode -- and rollupCrossTx is the
// path whose round gate does not stop a replay, so the count is the exposure.
//
// Field numbers are read from the generated pb.go files; plugin cannot be imported from chain33,
// so the payloads are walked by hand, the same way the token and anchor modes do it.
//
//	RollupAction   { ty = 1; oneof value { CheckPoint commit = 2 } }
//	CheckPoint     { chainTitle = 1, commitRound = 2, batch = 3, validatorPubs = 4 }   (+ syncedHeight = 6)
//	BlockBatch     { blockHeaders = 1, blockFragIndex = 2, txList = 3, crossTxCheckHash = 7, crossTxResults = 8 }
//	ParacrossAction{ oneof value { ... RollupCrossTx rollupCrossTx = 15 } }  ->  RollupCrossTx
//	RollupCrossTx  { chainTitle = 1, commitRound = 2, txIndices = 3 }
//	CrossTxIndex   { blockHeight = 1, filterIndex = 2, txHash = 3 }
const (
	rollupExecer    = "rollup"
	paracrossExecer = "paracross"

	rollupArmCommit = 2

	cpChainTitle   = 1
	cpCommitRound  = 2
	cpBatch        = 3
	cpValidatorPub = 4
	cpSyncedHeight = 6

	batchFragIndex = 2
	batchTxList    = 3
	batchCheckHash = 7
	batchResults   = 8

	paraArmRollupCrossTx = 15

	rctxChainTitle  = 1
	rctxCommitRound = 2
	rctxTxIndices   = 3

	ctxBlockHeight = 1
	ctxFilterIndex = 2
	ctxTxHash      = 3

	// maxHashesListed bounds the per-hit hash list so a wide round cannot fill the disk.
	maxHashesListed = 8
)

// rollupCP is the part of a CheckPoint this scan judges: which chain, which round, and how much
// the round carried.
type rollupCP struct {
	Title        string
	Round        int64
	FragIndex    int64
	TxListLen    int
	TxListBytes  int
	CheckHash    string
	Results      string
	SyncedHeight int64
	Validators   int
}

// rollupCommitHit is one execer-"rollup" transaction.
type rollupCommitHit struct {
	Kind    string `json:"kind"`
	Height  int64  `json:"height"`
	TxIndex int    `json:"tx_index"`
	Execer  string `json:"execer"`
	Receipt int32  `json:"receipt"`
	To      string `json:"to"`

	ChainTitle          string `json:"chain_title"`
	CommitRound         int64  `json:"commit_round"`
	BlockFragIndex      int64  `json:"block_frag_index"`
	TxListLen           int    `json:"tx_list_len"`
	CrossTxCheckHash    string `json:"cross_tx_check_hash"`
	CrossTxResults      string `json:"cross_tx_results"`
	CrossTxSyncedHeight int64  `json:"cross_tx_synced_height"`
	ValidatorPubs       int    `json:"validator_pubs"`
}

// rollupCrossTxHit is one paracross RollupCrossTx transaction -- the settlement of a round's
// cross-chain asset transfers.
type rollupCrossTxHit struct {
	Kind    string `json:"kind"`
	Height  int64  `json:"height"`
	TxIndex int    `json:"tx_index"`
	Execer  string `json:"execer"`
	Receipt int32  `json:"receipt"`
	To      string `json:"to"`

	ChainTitle  string   `json:"chain_title"`
	CommitRound int64    `json:"commit_round"`
	IndexCount  int      `json:"index_count"`
	TxHashes    []string `json:"tx_hashes,omitempty"`
	Truncated   bool     `json:"hashes_truncated,omitempty"`
}

// rollupTitleStat aggregates one chain title. RoundMin/RoundMax stand in for a round set: a
// title's rounds only advance, so the pair brackets what it ever committed.
type rollupTitleStat struct {
	Title          string `json:"title"`
	Commits        int64  `json:"commits"`
	RoundMin       int64  `json:"round_min"`
	RoundMax       int64  `json:"round_max"`
	CrossTxTxs     int64  `json:"rollup_cross_txs"`
	CrossTxIndices int64  `json:"cross_tx_indices"`
	ExecOk         int64  `json:"receipt_exec_ok"`
	Packed         int64  `json:"receipt_exec_pack"`
	Failed         int64  `json:"receipt_failed"`
	HeightMin      int64  `json:"height_min"`
	HeightMax      int64  `json:"height_max"`

	// RoundGaps counts commits whose round did not advance past the previous one, which is what a
	// replayed or duplicated commit looks like from the outside.
	RoundGaps int64 `json:"non_advancing_rounds"`
}

// rollupSummary is written once at the end of a rollup-mode run.
type rollupSummary struct {
	Kind           string             `json:"kind"`
	CommitTxs      int64              `json:"rollup_commit_txs"`
	CrossTxTxs     int64              `json:"rollup_cross_tx_txs"`
	CrossTxIndices int64              `json:"rollup_cross_tx_indices"`
	HeightMin      int64              `json:"height_min"`
	HeightMax      int64              `json:"height_max"`
	DecodeFailed   int64              `json:"decode_failed"`
	OtherParacross int64              `json:"other_paracross_txs_skipped"`
	Titles         []*rollupTitleStat `json:"titles"`
}

// rollupCollector holds the running aggregates for a rollup-mode scan.
type rollupCollector struct {
	sum   rollupSummary
	byT   map[string]*rollupTitleStat
	stats map[string]int64
}

func newRollupCollector(stats map[string]int64) *rollupCollector {
	return &rollupCollector{
		sum:   rollupSummary{Kind: "rollup_summary"},
		byT:   map[string]*rollupTitleStat{},
		stats: stats,
	}
}

func (c *rollupCollector) title(name string) *rollupTitleStat {
	t := c.byT[name]
	if t == nil {
		t = &rollupTitleStat{Title: name}
		c.byT[name] = t
	}
	return t
}

// rollupTx folds one transaction into the collector and reports whether it was a hit. It is
// called for every transaction in the window, so the execer is compared before any decoding.
func (c *rollupCollector) rollupTx(emit func(interface{}) error, tx *types.Transaction, height int64, txIndex int, receipt int32) (bool, error) {
	switch string(tx.GetExecer()) {
	case rollupExecer:
		cp, ok := rollupCheckPoint(tx.GetPayload())
		if !ok {
			c.stats["__rollup_decode_failed"]++
			c.sum.DecodeFailed++
			return false, nil
		}
		t := c.title(cp.Title)
		t.Commits++
		if t.RoundMax != 0 || t.RoundMin != 0 {
			if cp.Round <= t.RoundMax {
				t.RoundGaps++
			}
		}
		if t.RoundMin == 0 || cp.Round < t.RoundMin {
			t.RoundMin = cp.Round
		}
		if cp.Round > t.RoundMax {
			t.RoundMax = cp.Round
		}
		if t.HeightMin == 0 || height < t.HeightMin {
			t.HeightMin = height
		}
		if height > t.HeightMax {
			t.HeightMax = height
		}

		c.sum.CommitTxs++
		c.bumpRange(height)

		h := rollupCommitHit{
			Kind: "commit", Height: height, TxIndex: txIndex, Execer: rollupExecer,
			Receipt: receipt, To: tx.GetTo(),
			ChainTitle: cp.Title, CommitRound: cp.Round, BlockFragIndex: cp.FragIndex,
			TxListLen: cp.TxListLen, CrossTxCheckHash: cp.CheckHash,
			CrossTxResults: cp.Results, CrossTxSyncedHeight: cp.SyncedHeight,
			ValidatorPubs: cp.Validators,
		}
		return true, emit(h)

	case paracrossExecer:
		rc, ok := paracrossRollupCrossTx(tx.GetPayload())
		if !ok {
			c.sum.OtherParacross++
			return false, nil
		}
		t := c.title(rc.Title)
		t.CrossTxTxs++
		t.CrossTxIndices += int64(len(rc.Hashes))
		switch receipt {
		case types.ExecOk:
			t.ExecOk++
		case types.ExecPack:
			t.Packed++
		default:
			t.Failed++
		}
		if t.HeightMin == 0 || height < t.HeightMin {
			t.HeightMin = height
		}
		if height > t.HeightMax {
			t.HeightMax = height
		}

		c.sum.CrossTxTxs++
		c.sum.CrossTxIndices += int64(len(rc.Hashes))
		c.bumpRange(height)

		h := rollupCrossTxHit{
			Kind: "crosstx", Height: height, TxIndex: txIndex, Execer: paracrossExecer,
			Receipt: receipt, To: tx.GetTo(),
			ChainTitle: rc.Title, CommitRound: rc.Round,
			IndexCount: len(rc.Hashes),
		}
		// The first hash of a round is enough to recognise the round from another source; the
		// full list is bounded so a wide round cannot fill the output disk.
		if len(rc.Hashes) > maxHashesListed {
			h.TxHashes = rc.Hashes[:maxHashesListed]
			h.Truncated = true
		} else {
			h.TxHashes = rc.Hashes
		}
		return true, emit(h)
	}
	return false, nil
}

func (c *rollupCollector) bumpRange(height int64) {
	if c.sum.HeightMin == 0 || height < c.sum.HeightMin {
		c.sum.HeightMin = height
	}
	if height > c.sum.HeightMax {
		c.sum.HeightMax = height
	}
}

// write emits the per-title aggregates, then the summary.
func (c *rollupCollector) write(enc *json.Encoder) error {
	titles := make([]*rollupTitleStat, 0, len(c.byT))
	for _, t := range c.byT {
		titles = append(titles, t)
	}
	// Deterministic order: by descending transaction count, then title.
	for i := 1; i < len(titles); i++ {
		for j := i; j > 0; j-- {
			a, b := titles[j-1], titles[j]
			if a.Commits+a.CrossTxTxs >= b.Commits+b.CrossTxTxs && (a.Commits+a.CrossTxTxs > b.Commits+b.CrossTxTxs || a.Title <= b.Title) {
				break
			}
			titles[j-1], titles[j] = titles[j], titles[j-1]
		}
	}
	c.sum.Titles = titles
	return enc.Encode(c.sum)
}

// rollupCheckPoint decodes the CheckPoint inside a RollupAction commit arm.
func rollupCheckPoint(payload []byte) (rollupCP, bool) {
	var cp rollupCP
	commit, ok := protoBytes(payload, rollupArmCommit)
	if !ok {
		return cp, false
	}
	cp.Title, _ = protoString(commit, cpChainTitle)
	cp.Round, _ = protoInt(commit, cpCommitRound)
	cp.SyncedHeight, _ = protoInt(commit, cpSyncedHeight)
	cp.Validators = len(protoAllBytes(commit, cpValidatorPub))
	batch, ok := protoBytes(commit, cpBatch)
	if !ok {
		// A commit with no batch still identifies the title and round.
		return cp, cp.Title != "" || cp.Round != 0
	}
	cp.FragIndex, _ = protoInt(batch, batchFragIndex)
	txList := protoAllBytes(batch, batchTxList)
	cp.TxListLen = len(txList)
	for _, t := range txList {
		cp.TxListBytes += len(t)
	}
	if v, ok := protoBytes(batch, batchCheckHash); ok {
		cp.CheckHash = hex.EncodeToString(v)
	}
	if v, ok := protoBytes(batch, batchResults); ok {
		cp.Results = hex.EncodeToString(v)
	}
	return cp, true
}

// rollupCrossTxData is the decoded RollupCrossTx of a paracross transaction.
type rollupCrossTxData struct {
	Title  string
	Round  int64
	Hashes []string
}

// paracrossRollupCrossTx returns the RollupCrossTx payload when the transaction carries one, and
// ok=false for every other paracross action -- which is most of them.
func paracrossRollupCrossTx(payload []byte) (rollupCrossTxData, bool) {
	var rc rollupCrossTxData
	arm, ok := protoBytes(payload, paraArmRollupCrossTx)
	if !ok {
		return rc, false
	}
	rc.Title, _ = protoString(arm, rctxChainTitle)
	rc.Round, _ = protoInt(arm, rctxCommitRound)
	for _, ix := range protoAllBytes(arm, rctxTxIndices) {
		if h, ok := protoBytes(ix, ctxTxHash); ok {
			rc.Hashes = append(rc.Hashes, hex.EncodeToString(h))
		} else {
			rc.Hashes = append(rc.Hashes, "")
		}
	}
	return rc, true
}

// protoAllBytes collects every value of a repeated length-delimited field.
func protoAllBytes(msg []byte, field int) [][]byte {
	var out [][]byte
	for len(msg) > 0 {
		num, typ, n := protowire.ConsumeTag(msg)
		if n < 0 {
			return out
		}
		msg = msg[n:]
		if int(num) == field && typ == protowire.BytesType {
			v, n2 := protowire.ConsumeBytes(msg)
			if n2 < 0 {
				return out
			}
			out = append(out, v)
			msg = msg[n2:]
			continue
		}
		n2 := protowire.ConsumeFieldValue(num, typ, msg)
		if n2 < 0 {
			return out
		}
		msg = msg[n2:]
	}
	return out
}

// crossTxIndex is one CrossTxIndex entry: where in the para chain the referenced transaction sits.
type crossTxIndex struct {
	BlockHeight int64
	FilterIndex int32
	TxHash      string
}

// paracrossRollupCrossTxIndices decodes the same payload again, this time keeping the index
// entries whole. The hit output only carries hashes; a follow-up pass needs the heights.
func paracrossRollupCrossTxIndices(payload []byte) (string, int64, []crossTxIndex, bool) {
	arm, ok := protoBytes(payload, paraArmRollupCrossTx)
	if !ok {
		return "", 0, nil, false
	}
	title, _ := protoString(arm, rctxChainTitle)
	round, _ := protoInt(arm, rctxCommitRound)
	var idx []crossTxIndex
	for _, e := range protoAllBytes(arm, rctxTxIndices) {
		var ci crossTxIndex
		ci.BlockHeight, _ = protoInt(e, ctxBlockHeight)
		fi, _ := protoInt(e, ctxFilterIndex)
		ci.FilterIndex = int32(fi)
		if h, ok := protoBytes(e, ctxTxHash); ok {
			ci.TxHash = hex.EncodeToString(h)
		}
		idx = append(idx, ci)
	}
	return title, round, idx, true
}
