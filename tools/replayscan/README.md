# replayscan

Finds every transaction on a chain whose **replay can disagree with what the chain recorded**
— the ones that make a replayed block's state root stop matching the one on chain, stalling a
node that syncs from scratch.

A full sync of the bityuan mainnet takes days, and it only tells you about a divergence
*after* it stalls. This walks the same data in about half an hour and lists the candidates up
front, with the verdict the chain itself recorded, so the two can be compared.

Two classes are covered, in two modes:

| mode | class | what flips |
|---|---|---|
| `addr` | recipient addresses | the fork gates in `system/dapp/driver.go` decide whether a legacy address is accepted, so a build or fork-height change moves the verdict |
| `token` | token genesis amounts | `account.GenesisInit`'s amount bound, which chain33 v1.70.0 added and #1401 removed, decides whether a `finishCreate` succeeds |

**Scope the run to the class you changed.** A mode's candidates can only appear where its code
is in play — `addr` at a gate is only consulted below the fork height it gates, and the token
bound only matters where the amount sits past it — so run only over the heights the change can
reach. Scanning the whole chain "to be safe" multiplies the cost without making the answer any
more reliable; the risk that matters is picking the range wrong, and that is decided by the
change's own conditions, not by scanning more.

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

## The addr filter

**Read the verdict the way a replay does**, not the way one driver sees it:

```go
eStrict     := address.CheckAddress(addr, 0)           // fewest drivers enabled
ePermissive := address.CheckAddress(addr, ethEnable)   // most drivers enabled

candidate := (ePermissive == nil && eStrict != nil) ||                  // height-dependent
             (ePermissive != nil && ePermissive != address.ErrAddressLength) // error-dependent
```

Reading a single driver is a trap that cost a wrong report once: `CheckBase58Address(NormalVer,
addr)` says `ErrCheckVersion` for a **multisig** address, but `btcMultiSign` accepts it once that
driver is enabled, so the replay accepts it too. The tool called two live mainnet transactions a
stall risk on that basis; they were fine.

Two properties make the two extreme heights sufficient:

* a driver turns on at its `enableHeight` and never off, so the enabled set only grows with
  height — the permissive end bounds every higher verdict, the strict end bounds every lower one;
* at the strict end an `ErrAddressLength` cannot be rescued by anything (no driver accepts a
  string that is not an address), so those are dropped as noise. They were 264 of 272 hits
  before this filter existed.

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

## The token filter

`tokenFinishCreate` calls `account.GenesisInit(owner, token.GetTotal())` with the total the
matching preCreate wrote into state. chain33 v1.70.0 made `GenesisInit` run
`types.CheckAmount(amount, precision)`, which rejects `amount >= MaxCoin(1e9) *
coinPrecision(1e8) = 1e17`, while `preCreate` allows anything up to `MaxTokenBalance` (9e18).
A token whose total sits above `1e17` therefore cannot finish on a build carrying the bound,
but the chain recorded its finish as a success — so the replay diverges there. #1401 removed
the bound again.

The tool walks the chain in height order, remembers each `preCreate` symbol and total, and
reports the `finishCreate` transactions whose total is `>= 1e17` or negative. A token's symbol
is unique per chain and its preCreate always precedes its finishCreate, so the map is small
and safe to build as the walk advances.

**Only the execer asked for is decoded** — the main chain's `token` by default. A para chain's
token is `user.p.<title>.token`, a different dapp that the main chain does not execute: it
records every one of those transactions as `ExecPack`. They say nothing about the main chain's
token dapp and are counted under `__other_chain_token_skipped` instead of reported — on bityuan
they were 180 of 200 hits before this filter. Pass `-execer user.p.<title>.token` to scan a
para chain's own copy of the chain.

## Usage

The database is locked by leveldb while a node runs, so **stop the node first** — a read-only
open is still refused (`resource temporarily unavailable`).

```sh
# look at the on-disk layout of a table
go run ./tools/replayscan -dump /path/to/datadir/blockchain.db "CHAIN-body-body-d-"

# scan (prefix, and optional row limit / output cap for a quick or bounded trial)
go run ./tools/replayscan addr  /path/to/datadir/blockchain.db out.jsonl "CHAIN-body-body-d-"
go run ./tools/replayscan token /path/to/datadir/blockchain.db out.jsonl "CHAIN-body-body-d-"

# cap the output: stop once the hits reach maxout, so a wide filter cannot fill the disk
go run ./tools/replayscan addr /path/to/datadir/blockchain.db out.jsonl "CHAIN-body-body-d-" 0 100000

# a para chain's own token (default is the main chain's "token")
go run ./tools/replayscan token /path/to/datadir/blockchain.db out.jsonl "CHAIN-body-body-d-" 0 0 -execer user.p.fzmtest.token
```

Output is JSON lines, one per candidate:

```json
{"height":101641,"tx_index":2,"execer":"coins","to":"1Di16bUjPJnvZ8Hrf4vuQDffzkv9jC5Jp","receipt":1,
 "verdict_here":"Address Checksum error","verdict_h0":"Address Checksum error",
 "verdict_fork":"Address Checksum error","verdict_eth":"Address Checksum error"}
```

`addr` hits carry `receipt` and the four `verdict_*` fields: what `address.CheckAddress`
answers at the transaction's own height, at 0, at the btcMultiSign enable height and at the
eth enable height. A candidate whose verdicts differ across them is height-dependent; one
whose verdicts agree is decided by the error alone, which is the shape a gate change moves.

`token` hits carry the symbol, the total the preCreate recorded and `receipt`:

```json
{"height":394223,"tx_index":1,"execer":"token","symbol":"TEST","total":9000000000000000000,"receipt":2}
```

In both modes `receipt` is the type the block was produced with: 1 = `ExecPack`, 2 =
`ExecOk`, -1 = absent. Put it next to what the replay now decides: for an `addr` hit,
accepting an `ExecPack` or rejecting an `ExecOk` is a divergence; for a `token` hit, a
`finishCreate` the chain recorded as `ExecOk` diverges on any build that still carries the
`GenesisInit` bound.

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
* **A `token` run that starts above a token's preCreate.** The total comes from the preCreate
  seen earlier in the same walk, so a run scoped to a height range misses the finishCreate of
  any token created below that range — those show up in the `__finish_without_precreate`
  count. Start the token scan at height 0, or check that counter is zero before trusting the
  result.
* **Other `account.GenesisInit` callers** are not decoded — `coins` and `coinsx` genesis, the
  `js` mint path, and token's own `withdraw`/`transfer` genesis all pass an amount taken from
  their own payload, so the same bound applies to them. Only the token preCreate/finishCreate
  pair is covered.
