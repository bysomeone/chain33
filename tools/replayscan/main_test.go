package main

import (
	"testing"

	"github.com/33cn/chain33/types"
	"google.golang.org/protobuf/encoding/protowire"
)

func txFromPayload(t *testing.T, payload []byte) *types.Transaction {
	t.Helper()
	return &types.Transaction{Execer: []byte("token"), Payload: payload}
}

// The two token messages are walked by hand because plugin cannot be imported from chain33,
// and the field numbers differ between them (preCreate puts symbol at 2, finishCreate at 1).
// These tests pin that walk against payloads built field by field, so a wrong number shows up
// here rather than as a silently empty scan.

func str(payload []byte, field int, v string) []byte {
	payload = protowire.AppendTag(payload, protowire.Number(field), protowire.BytesType)
	return protowire.AppendString(payload, v)
}

func i64(payload []byte, field int, v int64) []byte {
	payload = protowire.AppendTag(payload, protowire.Number(field), protowire.VarintType)
	return protowire.AppendVarint(payload, uint64(v))
}

// arm wraps a oneof arm the way TokenAction does.
func arm(num int, body []byte) []byte {
	out := protowire.AppendTag(nil, protowire.Number(num), protowire.BytesType)
	return protowire.AppendBytes(out, body)
}

func preCreate(name, symbol string, total int64) []byte {
	var body []byte
	body = str(body, 1, name)
	body = str(body, 2, symbol)
	body = str(body, 3, "intro")
	body = i64(body, 4, total)
	body = i64(body, 5, 100)
	return arm(tokenPreCreate, body)
}

func finishCreate(symbol, owner string) []byte {
	var body []byte
	body = str(body, 1, symbol)
	body = str(body, 2, owner)
	return arm(tokenFinishCreate, body)
}

func TestTokenActionArm(t *testing.T) {
	arm, body, ok := tokenActionArm(preCreate("Test Token", "TEST", 1e18))
	if !ok || arm != tokenPreCreate {
		t.Fatalf("preCreate: arm=%d ok=%v, want %d/true", arm, ok, tokenPreCreate)
	}
	if sym, _ := protoString(body, 2); sym != "TEST" {
		t.Fatalf("preCreate symbol = %q, want TEST", sym)
	}
	if total, _ := protoInt(body, 4); total != 1e18 {
		t.Fatalf("preCreate total = %d, want 1e18", total)
	}

	// Ty = 7 rides outside the oneof and must be skipped, not mistaken for an arm.
	var withTy []byte
	withTy = append(withTy, finishCreate("TEST", "owner")...)
	withTy = i64(withTy, 7, 2)
	arm, body, ok = tokenActionArm(withTy)
	if !ok || arm != tokenFinishCreate {
		t.Fatalf("finishCreate: arm=%d ok=%v, want %d/true", arm, ok, tokenFinishCreate)
	}
	if sym, _ := protoString(body, 1); sym != "TEST" {
		t.Fatalf("finishCreate symbol = %q, want TEST", sym)
	}

	// A payload that is not a token action at all.
	if _, _, ok := tokenActionArm([]byte{0x08, 0x01}); ok {
		t.Fatal("a plain varint field was read as a token arm")
	}
}

func TestTokenScanReportsOnlyTotalsPastTheBound(t *testing.T) {
	cases := []struct {
		name  string
		total int64
		want  bool
	}{
		{"below the bound", maxCoinPrecision - 1, false},
		{"exactly the bound", maxCoinPrecision, true},
		{"above the bound", 9e18, true},
		{"negative", -1, true},
		{"zero", 0, false},
	}
	for _, tc := range cases {
		totals := map[string]int64{}
		stats := map[string]int64{}
		var emitted int
		emit := func(interface{}) error { emitted++; return nil }

		tx := txFromPayload(t, preCreate("Test Token", "TEST", tc.total))
		if got, err := tokenScan(emit, tx, 1, 0, 2, "token", totals, stats); err != nil || got {
			t.Fatalf("%s: preCreate reported a hit (got=%v err=%v)", tc.name, got, err)
		}
		if totals["TEST"] != tc.total {
			t.Fatalf("%s: recorded total = %d, want %d", tc.name, totals["TEST"], tc.total)
		}

		fin := txFromPayload(t, finishCreate("TEST", "owner"))
		got, err := tokenScan(emit, fin, 2, 1, 2, "token", totals, stats)
		if err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		if got != tc.want {
			t.Fatalf("%s: total=%d reported=%v, want %v", tc.name, tc.total, got, tc.want)
		}
		if emitted != b2i(tc.want) {
			t.Fatalf("%s: emitted %d hits, want %d", tc.name, emitted, b2i(tc.want))
		}
	}
}

func TestTokenScanIgnoresOtherExecers(t *testing.T) {
	totals := map[string]int64{"TEST": 9e18}
	stats := map[string]int64{}
	tx := txFromPayload(t, finishCreate("TEST", "owner"))
	tx.Execer = []byte("coins")

	if got, _ := tokenScan(func(interface{}) error { return nil }, tx, 1, 0, 2, "token", totals, stats); got {
		t.Fatal("a non-token execer was scanned")
	}
}

// A para chain's token is a different dapp, and the main chain records every one of its
// transactions as ExecPack -- on bityuan they were 180 of 200 hits before this filter. They
// must be skipped, counted, and not silently swallowed.
func TestTokenScanSkipsParaChainTokens(t *testing.T) {
	totals := map[string]int64{"TEST": 9e18}
	stats := map[string]int64{}
	tx := txFromPayload(t, finishCreate("TEST", "owner"))
	tx.Execer = []byte("user.p.fzmtest.token")
	emitted := 0

	got, err := tokenScan(func(interface{}) error { emitted++; return nil }, tx, 1, 0, 1, "token", totals, stats)
	if err != nil || got || emitted != 0 {
		t.Fatalf("para-chain token reported (got=%v err=%v emitted=%d)", got, err, emitted)
	}
	if stats["__other_chain_token_skipped"] != 1 {
		t.Fatalf("skip not counted: stats=%v", stats)
	}

	// The same transaction is the point of the run when the para chain's execer is asked for.
	got, err = tokenScan(func(interface{}) error { emitted++; return nil }, tx, 1, 0, 1, "user.p.fzmtest.token", totals, stats)
	if err != nil || !got || emitted != 1 {
		t.Fatalf("-execer user.p.fzmtest.token did not report it (got=%v err=%v emitted=%d)", got, err, emitted)
	}
}

// para mode is a density histogram, so what matters is which transactions land in which
// height bucket. Both the main chain's own paracross and a para chain's namespaced copy
// reach the same block-fetching code, so both must be counted.
func TestParaCountBucketsByHeight(t *testing.T) {
	buckets := map[int64]map[string]int64{}

	// want bucket is which 100k band the height must land in; -1 means "not paracross".
	cases := []struct {
		execer string
		height int64
		want   int64
	}{
		{"paracross", 1, 0},
		{"user.p.bscdex.paracross", 18473244, 184},
		{"paracross", 18499999, 184},
		{"paracross", 18500000, 185}, // exactly the band boundary
		{"paracross", 0, 0},
		{"coins", 18473244, -1},
		{"token", 18473244, -1},
		{"", 18473244, -1},
	}
	for _, tc := range cases {
		got := paraCount(buckets, tc.execer, tc.height)
		if want := tc.want >= 0; got != want {
			t.Fatalf("paraCount(%q, %d) = %v, want %v", tc.execer, tc.height, got, want)
		}
	}

	if got := buckets[0]["paracross"]; got != 2 {
		t.Fatalf("bucket 0 paracross = %d, want 2 (heights 0 and 1)", got)
	}
	if got := buckets[184]["user.p.bscdex.paracross"]; got != 1 {
		t.Fatalf("bucket 184 para count = %d, want 1", got)
	}
	// The para chain's copy and the main chain's own executor are different keys, so a
	// bucket's total is the sum over execers, not one number.
	if got := buckets[184]["paracross"]; got != 1 {
		t.Fatalf("bucket 184 paracross = %d, want 1 (18499999 only)", got)
	}
	if got := buckets[185]["paracross"]; got != 1 {
		t.Fatalf("bucket 185 paracross = %d, want 1", got)
	}
	if len(buckets) != 3 {
		t.Fatalf("buckets = %d, want 3 (0, 184, 185)", len(buckets))
	}
}

func b2i(b bool) int {
	if b {
		return 1
	}
	return 0
}
