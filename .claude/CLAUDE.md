# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## Build & Test Commands

```bash
make                  # build chain33 + chain33-cli → build/
make test             # go test -short -race (all packages)
make test PKG=./queue # run a single package test
make linter           # golangci-lint + go vet + ineffassign + gosec
make fmt              # gofmt + goimports across all Go files
make proto            # regenerate protobuf (.pb.go from .proto)
make mock             # regenerate mock interfaces (mockery)
```

## Architecture

Chain33 is a modular blockchain framework. All inter-module communication goes through the **message queue** — modules never call each other directly.

### Message Queue (queue package)

The queue is a pub/sub bus. Each module subscribes to a **topic** (string key) with exactly one subscriber per topic. Modules communicate by sending typed messages to topics and optionally waiting for a reply.

- **Two priority channels per topic**: `high` (buffer 64, synchronous) and `low` (buffer 40960, async). The subscriber goroutine drains high first, then low.
- **Reply mechanism**: Each message carries its own `chReply chan *Message`. The receiver calls `msg.Reply(replyMsg)` to respond. The sender calls `client.Wait(msg)` to block for the reply.
- **Queue lifecycle**: `New(name)` creates → `Client()` returns a client → `client.Sub("topic")` subscribes → `q.Start()` blocks until `q.Close()` or `q.CloseQueue()` is called.
- **Message pooling**: `NewMessage` uses `sync.Pool`; always call `client.FreeMessage(msg)` after processing.
- **Closing sequence**: Send close signals (non-blocking) → `close(sub.done)` → set `isClose=1` on the existing `chanSub` (never replace it with a new object — nil channels cause deadlocks in `WaitTimeout`).

### Module Registration (init-based)

All built-in modules register themselves via `init()` functions in the `system/` tree. The blank import `_ "github.com/33cn/chain33/system"` in `cmd/chain33/main.go` triggers all registrations.

Each module implements `queue.Module` (`SetQueueClient`, `Wait`, `Close`) and gets a queue client injected at startup.

Dapps (smart contracts) implement the `Driver` interface (`system/dapp/driver.go`): `Exec`, `ExecLocal`, `ExecDelLocal`, `CheckTx`, `Query`.

Crypto drivers register via `cryptocli.RegisterCryptoHandler(eventType, handler)` and `cryptocli.RegisterSubInitFunc(name, initFunc)`.

### Event System

All message types are constants in `types/event.go` (~180 events). Each event maps to a topic and a protobuf type. `types.GetEventName(int)` returns human-readable names for logging. Reply events use `types.EventReply`.

### Key Packages

| Package | Role |
|----------|------|
| `types/` | Protobuf types, config, events, errors, fork rules |
| `queue/` | Message bus |
| `blockchain/` | Block storage, chain sync, orphan management |
| `executor/` | Transaction execution engine |
| `mempool/` | Transaction pool with account indexing |
| `consensus/` | Consensus interface |
| `store/` | State store (Merkle AVL tree) |
| `wallet/` | Wallet management |
| `client/` | High-level queue protocol helpers |
| `rpc/` | JSON-RPC + gRPC servers |
| `common/` | Shared: crypto, DB (Badger/LevelDB), logging (log15) |
| `pluginmgr/` | Plugin registration and lifecycle |
| `system/` | Built-in plugin implementations |
| `util/` | CLI framework (Cobra), test utilities |

### Crypto / TSS

The TSS subsystem (`system/crypto/tss/`) implements GG18 threshold ECDSA signing via the `getamis/alice` library. Requires `Crypto.EnableTSS = true` in config.

- **PeerManager**: Discovers peers via P2P's `EventPeerInfo`, validates rank combinations (Birkhoff interpolation), routes protocol messages through the queue.
- **GG18 handlers**: Three protocols under `gg18/` — DKG (`/gg18/dkg`), Sign (`/gg18/sign`), Reshare (`/gg18/reshare`).
- **Message flow**: P2P → `EventCryptoTssMsg` → `dispatchMessage` → worker `handleTssMsg` → protocol-specific handler → Alice GG18 core.
- **Session management**: Concurrent sessions tracked per protocol. Cleanup via `removeSession` on completion/timeout.

### Config & Forks

Blockchain rules change at specific heights via the fork mechanism (`types/fork.go`). `types.Chain33Config` holds all configuration. `types.AssertConfig(q)` panics if the queue has no config set — always call `queue.SetConfig(cfg)` early.

### Project Conventions

- Go 1.20+, module path `github.com/33cn/chain33`
- `gofmt` + `goimports` mandatory before commits
- Tests use `github.com/stretchr/testify/assert`; run with `-race`
- Error wrapping: `fmt.Errorf("context: %w", err)`
- Functional options pattern for constructors (see `gg18/config.go`)
- External plugin repo: `github.com/33cn/plugin`
