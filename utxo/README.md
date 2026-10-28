# keymaster.cc-cli/utxo

Deterministic UTXO manager kernel + trusted submit orchestration for
contract `utm/1`. No external dependencies. The wallet maintains its TypeScript
implementation in `../../keymaster.cc/packages/utxo-manager/src/`. This package
belongs to the CLI root Go module; it has no separate module or TypeScript copy. Shared business vectors and state exchange are tested
in the engineering repository; internal defensive checks need not be identical.

## Use

```go
import "github.com/bsv8/keymaster.cc-cli/utxo"

kernel, _ := utxo.NewFundingKernel(utxo.Scope{Owner: ownerHex, Network: "test"},
    utxo.ManagerConfig{WalletScriptHex: walletScript, DustThresholdSats: "1", MaxUnconfirmedAncestors: 8})

kernel.ApplySnapshot(utxo.SnapshotInput{Generation: 1, Revision: 1, Utxos: [...]})
view, _ := kernel.Request(utxo.RequestInput{RequestID: "r1", BusinessAmount: "800",
    FeeRate: "10", Outputs: []utxo.RequestOutput{{ScriptHex: script, Value: "800"}}})
// Build the template, have the trusted signer sign it, then:
kernel.Prepare(utxo.PrepareInput{AllocationID: "r1", UnsignedTxHex: unsigned})
kernel.CompletePrepare(utxo.CompletePrepareInput{AllocationID: "r1", SignedTxHex: signed})
// Persist the exact bytes BEFORE broadcasting, then:
utxo.SubmitSigned(kernel, utxo.Ports{Store: store, Broadcaster: bc, Chain: chain}, "r1", signed)
```

The Go host adapter (keymaster.cc-cli wallet) owns the
`DurableStore`/`Broadcaster`/`ChainObserver` ports. Applications never see
signing or broadcast capabilities.

See `../../BSV8_Engineering/docs/keymaster.cc-cli/utxo-manager/` for established
facts and `../../BSV8_Engineering/todo/keymaster.cc-cli/utxo-manager/` for the
contract draft and施工 records. Tests and E2E live in the engineering repo,
not here.
