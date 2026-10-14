package main

import (
	"bytes"
	"encoding/hex"
	"testing"

	"github.com/33cn/chain33/types"
	"google.golang.org/protobuf/encoding/protowire"
)

// The payloads below are assembled with the field numbers the plugin's own .proto files declare,
// so a decoder that reads a different field is caught here instead of silently reporting zero
// hits on a chain that has them.

// mkTx builds the minimum a Transaction needs for this scan to recognise it.
func mkTx(execer string, payload []byte) *types.Transaction {
	return &types.Transaction{Execer: []byte(execer), Payload: payload}
}

func pbBytes(field int, v []byte) []byte {
	b := protowire.AppendTag(nil, protowire.Number(field), protowire.BytesType)
	return protowire.AppendBytes(b, v)
}

func pbVarint(field int, v int64) []byte {
	b := protowire.AppendTag(nil, protowire.Number(field), protowire.VarintType)
	return protowire.AppendVarint(b, uint64(v))
}

func pbStr(field int, v string) []byte { return pbBytes(field, []byte(v)) }

// paracrossRollupCrossTxPayload builds ParacrossAction{ value: { rollupCrossTx = 15 } }.
func paracrossRollupCrossTxPayload(t string, round int64, idx [][3]interface{}) []byte {
	var inner []byte
	inner = append(inner, pbStr(rctxChainTitle, t)...)
	inner = append(inner, pbVarint(rctxCommitRound, round)...)
	for _, e := range idx {
		var one []byte
		one = append(one, pbVarint(ctxBlockHeight, e[0].(int64))...)
		one = append(one, pbVarint(ctxFilterIndex, int64(e[1].(int32)))...)
		one = append(one, pbBytes(ctxTxHash, e[2].([]byte))...)
		inner = append(inner, pbBytes(rctxTxIndices, one)...)
	}
	// ParacrossAction carries the oneof arm and, outside it, ty = 2.
	action := pbBytes(paraArmRollupCrossTx, inner)
	return append(action, pbVarint(paraActionTy, 113)...)
}

// rollupCommitPayload builds RollupAction{ commit: CheckPoint{...} }.
func rollupCommitPayload(t string, round, synced int64, txList [][]byte, hashes int) []byte {
	var batch []byte
	batch = append(batch, pbVarint(batchFragIndex, 0)...)
	for _, tx := range txList {
		batch = append(batch, pbBytes(batchTxList, tx)...)
	}
	batch = append(batch, pbBytes(batchCheckHash, bytes.Repeat([]byte{0x11}, 32))...)
	batch = append(batch, pbBytes(batchResults, []byte{0x05})...)

	var cp []byte
	cp = append(cp, pbStr(cpChainTitle, t)...)
	cp = append(cp, pbVarint(cpCommitRound, round)...)
	cp = append(cp, pbBytes(cpBatch, batch)...)
	for i := 0; i < hashes; i++ {
		cp = append(cp, pbBytes(cpValidatorPub, bytes.Repeat([]byte{byte(i)}, 33))...)
	}
	cp = append(cp, pbVarint(cpSyncedHeight, synced)...)
	return append(pbVarint(1, 1), pbBytes(rollupArmCommit, cp)...) // ty = Commit, then the arm
}

func TestParacrossRollupCrossTxDecode(t *testing.T) {
	h0 := bytes.Repeat([]byte{0xa0}, 32)
	h1 := bytes.Repeat([]byte{0xb1}, 32)
	payload := paracrossRollupCrossTxPayload("user.p.superx.", 42, [][3]interface{}{
		{int64(25771234), int32(2), h0},
		{int64(25771240), int32(0), h1},
	})

	rc, ok := paracrossRollupCrossTx(payload)
	if !ok {
		t.Fatal("arm 15 not recognised")
	}
	if rc.Title != "user.p.superx." {
		t.Errorf("title = %q", rc.Title)
	}
	if rc.Round != 42 {
		t.Errorf("round = %d", rc.Round)
	}
	want := []string{hex.EncodeToString(h0), hex.EncodeToString(h1)}
	if len(rc.Hashes) != 2 || rc.Hashes[0] != want[0] || rc.Hashes[1] != want[1] {
		t.Errorf("hashes = %v", rc.Hashes)
	}

	title, round, idx, ok := paracrossRollupCrossTxIndices(payload)
	if !ok || title != "user.p.superx." || round != 42 {
		t.Fatalf("indices decode: %v %q %d", ok, title, round)
	}
	if len(idx) != 2 {
		t.Fatalf("indices = %d", len(idx))
	}
	if idx[0].BlockHeight != 25771234 || idx[0].FilterIndex != 2 || idx[0].TxHash != want[0] {
		t.Errorf("idx[0] = %+v", idx[0])
	}
	if idx[1].BlockHeight != 25771240 || idx[1].FilterIndex != 0 {
		t.Errorf("idx[1] = %+v", idx[1])
	}
}

// A paracross Commit is the transaction shape that dominates the chain; it must not be mistaken
// for a rollup cross tx.
func TestParacrossCommitIsNotRollupCrossTx(t *testing.T) {
	status := append(pbVarint(paraStatusMainBlockHeight, 25770000), pbStr(paraStatusTitle, "user.p.superx.")...)
	commit := pbBytes(paraCommitStatus, status)
	payload := append(pbBytes(paraActionCommit, commit), pbVarint(paraActionTy, 1)...)

	if _, ok := paracrossRollupCrossTx(payload); ok {
		t.Fatal("a Commit was decoded as a RollupCrossTx")
	}
}

func TestRollupCheckPointDecode(t *testing.T) {
	payload := rollupCommitPayload("user.p.superx.", 7, 25771999, [][]byte{{0x01, 0x02}, {0x03}}, 3)

	cp, ok := rollupCheckPoint(payload)
	if !ok {
		t.Fatal("commit arm not recognised")
	}
	if cp.Title != "user.p.superx." {
		t.Errorf("title = %q", cp.Title)
	}
	if cp.Round != 7 {
		t.Errorf("round = %d", cp.Round)
	}
	if cp.TxListLen != 2 || cp.TxListBytes != 3 {
		t.Errorf("txList = %d entries / %d bytes", cp.TxListLen, cp.TxListBytes)
	}
	if cp.CheckHash != hex.EncodeToString(bytes.Repeat([]byte{0x11}, 32)) {
		t.Errorf("checkHash = %s", cp.CheckHash)
	}
	if cp.Results != "05" {
		t.Errorf("results = %s", cp.Results)
	}
	if cp.Validators != 3 {
		t.Errorf("validators = %d", cp.Validators)
	}
	if cp.SyncedHeight != 25771999 {
		t.Errorf("syncedHeight = %d", cp.SyncedHeight)
	}
}

// Every transaction carrying the rollup execer decodes or is counted as a failure; neither path
// may silently drop it.
func TestRollupCollectorCounts(t *testing.T) {
	stats := map[string]int64{}
	c := newRollupCollector(stats)
	emit := func(interface{}) error { return nil }

	commit := mkTx(rollupExecer, rollupCommitPayload("user.p.superx.", 1, 0, nil, 1))
	hit, err := c.rollupTx(emit, commit, 25770100, 0, 2, []int64{2, 101, 102})
	if err != nil || !hit {
		t.Fatalf("commit not counted: hit=%v err=%v", hit, err)
	}
	if c.sum.CommitTxs != 1 || len(c.byT) != 1 {
		t.Fatalf("summary = %+v", c.sum)
	}
	if t0 := c.byT["user.p.superx."]; t0 == nil || t0.Commits != 1 || t0.HeightMin != 25770100 {
		t.Fatalf("title stat = %+v", t0)
	}

	// A second commit whose round goes backwards is what a duplicate or a replayed commit looks
	// like from outside.
	commit2 := mkTx(rollupExecer, rollupCommitPayload("user.p.superx.", 1, 0, nil, 1))
	if _, err = c.rollupTx(emit, commit2, 25770101, 1, 2, []int64{2, 101, 102}); err != nil {
		t.Fatal(err)
	}
	if t0 := c.byT["user.p.superx."]; t0.RoundGaps != 1 {
		t.Errorf("RoundGaps = %d, want 1", t0.RoundGaps)
	}

	rc := mkTx(paracrossExecer, paracrossRollupCrossTxPayload("user.p.superx.", 1, [][3]interface{}{
		{int64(25770090), int32(0), bytes.Repeat([]byte{0xcd}, 32)},
	}))
	hit, err = c.rollupTx(emit, rc, 25770102, 2, 2, []int64{2, 21})
	if err != nil || !hit {
		t.Fatalf("cross tx not counted: hit=%v err=%v", hit, err)
	}
	if c.sum.CrossTxTxs != 1 || c.sum.CrossTxIndices != 1 {
		t.Fatalf("summary = %+v", c.sum)
	}
	if t0 := c.byT["user.p.superx."]; t0.CrossTxTxs != 1 || t0.ExecOk != 1 {
		t.Fatalf("title stat = %+v", t0)
	}

	// Anything else on the paracross execer is skipped, and counted so a zero result can be told
	// apart from a scan that never reached the execer.
	other := mkTx(paracrossExecer, pbBytes(paraActionCommit, []byte{}))
	if hit, err = c.rollupTx(emit, other, 25770103, 3, 2, []int64{2}); err != nil || hit {
		t.Fatalf("other paracross tx counted: hit=%v err=%v", hit, err)
	}
	if c.sum.OtherParacross != 1 {
		t.Errorf("otherParacross = %d", c.sum.OtherParacross)
	}
}
