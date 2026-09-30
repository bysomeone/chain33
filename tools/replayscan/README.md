# replayscan

Finds every transaction on a chain whose **recipient address would be judged differently by
different chain33 builds** — the addresses that can make a replayed block's state root stop
matching the one on chain, stalling a node that syncs from scratch.

A full sync of the bityuan mainnet takes days, and it only tells you about a flip *after* it
stalls. This walks the same data in about half an hour and lists the candidates up front,
with the verdict the chain itself recorded, so the two can be compared.

## Background: why address verdicts can flip

Before `Exec` runs a transaction, `execenv.go` checks the recipient through
`system/dapp/driver.go`'s `CheckAddress`, which tolerates some validity errors below the fork
heights so that historical blocks still replay:

```go
if !IsFork(height, "ForkMultiSignAddress") && err == ErrCheckVersion        { return nil }
if !IsFork(height, "ForkBase58AddressCheck") &&
   (err == ErrAddressChecksum || err == ErrCheckChecksum)                  { return nil }
return err
```

On bityuan both forks sit at `2270000`, so below that height the gates are on, at and above
it they are off and nothing is tolerated.

Two things about the raw check make its verdict build-dependent:

* **`common/address.CheckAddress` used to walk the drivers through a map**, so for an address
  several drivers reject, *which* error it reported was random. `eecef340d` made the walk
  ordered (ascending driver id), so the btc driver's error always wins.
* **The address cache was keyed by the address alone**, so a verdict computed at one height
  was reused at another — a query at `height = -1` (which enables every driver) could decide
  a later check at a real height. `9f230f1e9` keyed it by the enabled-driver set too.
* **`566304371` widened the gate** to tolerate `ErrCheckChecksum` as well. That accepts
  transactions the chain packed as *failures* — e.g. block 101641, where a coins transfer to
  a 25-byte address with a broken checksum was recorded as `ExecPack`. Replaying it executes
  the transfer (the coins path no longer validates the recipient's checksum) and the state
  root diverges. Reverting that one hunk is what this tool was written to justify.

## The filter

One call decides it:

```go
e := address.CheckBase58Address(address.NormalVer, addr)
if e != nil && e != address.ErrAddressLength { /* candidate */ }
```

A valid address of the normal version returns nil, and anything shorter than 25 bytes returns
`ErrAddressLength`; both verdicts agree across the builds, so only what is left can flip.
Candidates are then graded by the error, which is what the gates match on:

| error | shape | tolerated below the fork by |
|---|---|---|
| `ErrCheckVersion` | version byte ≠ 0x00 | the `ForkMultiSignAddress` gate |
| `ErrAddressChecksum` | len > 25, checksum bad | the `ForkBase58AddressCheck` gate |
| `ErrCheckChecksum` | len == 25, checksum bad | **only** the extra tolerance `566304371` added |

**Reading the output** — put each hit next to the receipt the block was produced with:

| chain receipt | replay accepts | replay rejects |
|---|---|---|
| `ExecOk` (2) | consistent | **diverges** — a transfer the chain made is skipped |
| `ExecPack` (1) | **diverges** — a transfer the chain never made is executed | consistent |

So an `ErrCheckChecksum` hit with `receipt: 1` is exactly the 101641 case: the chain failed it,
so the replay must fail it too.

## Usage

The database is locked by leveldb while a node runs, so **stop the node first** — a read-only
open is still refused (`resource temporarily unavailable`).

```sh
# look at the on-disk layout of a table
go run ./tools/replayscan -dump /path/to/datadir/blockchain.db "CHAIN-body"

# scan (prefix, and an optional row limit for a quick trial)
go run ./tools/replayscan /path/to/datadir/blockchain.db out.jsonl CHAIN-body
go run ./tools/replayscan /path/to/datadir/blockchain.db out.jsonl CHAIN-body 20000
```

Output is JSON lines, one per candidate:

```json
{"height":101641,"tx_index":2,"execer":"coins","to":"1Di16bUjPJnvZ8Hrf4vuQDffzkv9jC5Jp","err":"Address Checksum error","receipt":1}
```

`receipt` is the type the block was produced with: 1 = `ExecPack`, 2 = `ExecOk`, -1 = absent.

## How it reads the data

Block bodies live in the `CHAIN-body` table, wrapped by the `table` package:

```
CHAIN-body-body-d-<height:12 digits><hash:32 bytes>
└─── key ─────────────────────────────────────────┘

value = [8-byte little-endian length][height:12][hash:32][BlockBody protobuf]
        └──────────── 52-byte row header, skipped ───────────┘
```

The height is read from the key, which is also what makes the walk sequential. Keys sort by
height, so the scan reads the database in chain order — that is why 4700M rows take minutes
rather than hours. The entries under the older `Body:` prefix are dead: `GetLocalDBKeyList`
notes that `bodyPrefix`, `headerPrefix` and `heightToHeaderPrefix` are no longer used.

Two fields in the output come from the value: `tx.To` (the address the check sees — on a
non-parallel chain `GetRealToAddr()` returns `tx.To`) and `Receipts[i].Ty`.

## Not covered

* **Addresses inside payloads** (`hashlock`, `multisig`, `ticket`, `token`, `retrieve`,
  `autonomy`, `manage`) are validated by their own executors calling the raw
  `address.CheckAddress` — they bypass the `dapp.CheckAddress` gates entirely, so the gate
  changes above do not reach them. Their verdicts still move with the driver-order and
  cache changes, so they are worth a separate pass if that is what is being judged.
* **`manage`'s `Exec_Modify`** calls the raw check with no fork tolerance at all
  (`ForkManageExec` = 100000, already active), so a legacy address there is rejected by every
  build — a pre-existing condition, not something these changes introduced.
* **`GetRealToAddr()` on a parallel chain** resolves to a payload address; this tool reads
  `tx.To` only.
