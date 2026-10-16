package main

import (
	"bytes"
	"fmt"
	"strconv"
	"strings"

	"github.com/33cn/chain33/types"
	"github.com/syndtr/goleveldb/leveldb"
	"google.golang.org/protobuf/encoding/protowire"
	"google.golang.org/protobuf/proto"
)

// The body row is the table package's row: an 8-byte length, the 12-byte height, then the
// 32-byte block hash, followed by the BlockBody protobuf (same layout replayscan walks).
const (
	bodyRowPrefix            = "CHAIN-body-body-d-"
	bodyRowHeader            = 52
	paraActionTy             = 2
	tyParaAssetTransfer      = 10000
	tyParaAssetWithdraw      = 10001
	tyParaCrossAssetTransfer = 10005
)

// detailRow is one (height, block hash) the paratx table says holds transactions of a title.
type detailRow struct {
	Height  int64
	Hash    []byte
	TxCount int32
}

// tyStat counts a title's transactions by ParacrossAction.Ty, and how many of the chain's own
// paracross transactions name the title (the deposit direction, which the paratx table does not
// index by title).
type tyStat struct {
	ParaTxs     int64
	ParaByTy    map[int64]int64
	ParaHeights map[int64][]int64 // first few heights seen per interesting Ty
	MainTxs     int64
	MainByTy    map[int64]int64
	MainHeights map[int64][]int64
	Blocks      int64
	DecodeErr   int64
}

func newTyStat() *tyStat {
	return &tyStat{
		ParaByTy:    map[int64]int64{},
		ParaHeights: map[int64][]int64{},
		MainByTy:    map[int64]int64{},
		MainHeights: map[int64][]int64{},
	}
}

// isAssetTy reports whether a ParacrossAction.Ty moves a cross-chain asset.
func isAssetTy(ty int64) bool {
	return ty == tyParaAssetTransfer || ty == tyParaAssetWithdraw || ty == tyParaCrossAssetTransfer
}

func addHeight(m map[int64][]int64, ty, height int64) {
	if len(m[ty]) < 5 {
		m[ty] = append(m[ty], height)
	}
}

// scanDetails reads the blocks the collected rows point at and counts each title's transactions
// by type. Only those blocks are read, which is what makes this affordable: the paratx table
// already told us which heights can hold the title's transactions.
func scanDetails(db *leveldb.DB, rows map[string][]detailRow) map[string]*tyStat {
	out := map[string]*tyStat{}
	for title, list := range rows {
		st := newTyStat()
		out[title] = st
		// The table's title is already the full execer prefix ("user.p.<name>."), so it is used
		// as-is; adding "user.p." again matches nothing and reports zero everywhere.
		prefix := title
		for _, r := range list {
			key := append([]byte(bodyRowPrefix+fmt.Sprintf("%012d", r.Height)), r.Hash...)
			raw, err := db.Get(key, nil)
			if err != nil || len(raw) < bodyRowHeader {
				st.DecodeErr++
				continue
			}
			var body types.BlockBody
			if err := proto.Unmarshal(raw[bodyRowHeader:], &body); err != nil {
				st.DecodeErr++
				continue
			}
			st.Blocks++
			for _, tx := range body.Txs {
				exec := string(tx.GetExecer())
				switch {
				case strings.HasPrefix(exec, prefix):
					st.ParaTxs++
					ty, _ := protoIntField(tx.GetPayload(), paraActionTy)
					st.ParaByTy[ty]++
					if isAssetTy(ty) {
						addHeight(st.ParaHeights, ty, r.Height)
					}
				case exec == "paracross":
					// A main-chain paracross transaction names the para chain it moves value to
					// or from inside its payload; the paratx table does not index those by title.
					if bytes.Contains(tx.GetPayload(), []byte(prefix)) {
						st.MainTxs++
						ty, _ := protoIntField(tx.GetPayload(), paraActionTy)
						st.MainByTy[ty]++
						if isAssetTy(ty) {
							addHeight(st.MainHeights, ty, r.Height)
						}
					}
				}
			}
		}
	}
	return out
}

// protoIntField reads a varint field out of a protobuf message, for the payloads this tool only
// needs one number from.
func protoIntField(msg []byte, field int) (int64, bool) {
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

// detailRows is the collected paratx rows, kept so the report can show what the table said
// next to what the blocks actually contained.
var detailRows map[string][]detailRow

func rowsFor(title string) []detailRow { return detailRows[title] }

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

func printDetails(detailTitles []string, out map[string]*tyStat) {
	for _, title := range detailTitles {
		st := out[title]
		if st == nil {
			fmt.Printf("\n== %s: no paratx rows\n", title)
			continue
		}
		fmt.Printf("\n== %s ==\n  paratx rows=%d blocks_read=%d para_txs=%d para_by_ty=%v\n",
			title, len(rowsFor(title)), st.Blocks, st.ParaTxs, sortedTy(st.ParaByTy))
		for i, r := range rowsFor(title) {
			if i >= 6 && i < len(rowsFor(title))-2 {
				if i == 6 {
					fmt.Printf("    ... %d more rows ...\n", len(rowsFor(title))-8)
				}
				continue
			}
			fmt.Printf("    row height=%d table_tx_count=%d block_hash=%x\n", r.Height, r.TxCount, r.Hash[:min(8, len(r.Hash))])
		}
		for ty, hs := range st.ParaHeights {
			fmt.Printf("    asset ty=%d para heights %v\n", ty, hs)
		}
		fmt.Printf("  main_chain paracross txs naming it=%d by_ty=%v\n", st.MainTxs, sortedTy(st.MainByTy))
		for ty, hs := range st.MainHeights {
			fmt.Printf("    asset ty=%d main heights %v\n", ty, hs)
		}
		if st.DecodeErr > 0 {
			fmt.Printf("  rows that could not be read/decoded: %d\n", st.DecodeErr)
		}
	}
}

func sortedTy(m map[int64]int64) string {
	keys := make([]int64, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	for i := 1; i < len(keys); i++ {
		for j := i; j > 0 && keys[j] < keys[j-1]; j-- {
			keys[j], keys[j-1] = keys[j-1], keys[j]
		}
	}
	var b strings.Builder
	b.WriteString("{")
	for i, k := range keys {
		if i > 0 {
			b.WriteString(" ")
		}
		b.WriteString(strconv.FormatInt(k, 10) + ":" + strconv.FormatInt(m[k], 10))
	}
	b.WriteString("}")
	return b.String()
}
