// Package utxo implements the deterministic UTXO manager kernel and the
// trusted submit orchestration for contract utm/1.
//
// The kernel is pure: no clock, network, keys or filesystem. One instance
// serves one scope (owner + network). Amounts are canonical decimal strings
// at the JSON boundary and checked uint64 values inside. The TypeScript
// implementation in ../typescript is behavior-identical: same selection,
// fees, validation, errors and byte-identical serialization.
package utxo

import "fmt"

// ContractVersion is the implemented contract.
const ContractVersion = "utm/1"

// MaxMoneySats is the maximum BSV money in satoshis.
const MaxMoneySats uint64 = 2100000000000000

// BytesPerKB defines the fee unit: satoshis per 1000 bytes.
const BytesPerKB uint64 = 1000

// P2PKHInputWeight is the per-input signed-size upper bound (proven in P4).
const P2PKHInputWeight = 148

// Error codes (utm/1). Identical to the TypeScript implementation.
const (
	ErrInsufficient            = "insufficient"
	ErrNoUTXOs                 = "no-utxos"
	ErrPolicyDenied            = "policy-denied"
	ErrAmountInvalid           = "amount-invalid"
	ErrFeeInvalid              = "fee-invalid"
	ErrIDConflict              = "id-conflict"
	ErrScopeMismatch           = "scope-mismatch"
	ErrSnapshotStale           = "snapshot-stale"
	ErrSnapshotBindingRequired = "snapshot-binding-required"
	ErrSnapshotInputInvalid    = "snapshot-input-invalid"
	ErrFundsProtected          = "funds-protected"
	ErrMixedInputsUnsupported  = "mixed-inputs-unsupported"
	ErrUnknownInput            = "unknown-input"
	ErrSignatureExceedsBound   = "signature-exceeds-bound"
	ErrFeeOutOfRange           = "fee-out-of-range"
	ErrAlreadyKnown            = "already-known"
	ErrNotDispatched           = "not-dispatched"
	ErrIsolatedUnknown         = "isolated-unknown"
	ErrAncestorUnknown         = "ancestor-unknown"
	ErrAncestorConflict        = "ancestor-conflict"
	ErrAlreadyOccupied         = "already-occupied"
	ErrAllocationUnknown       = "allocation-unknown"
	ErrAllocationState         = "allocation-state"
	ErrVersionUnsupported      = "version-unsupported"
	ErrStateConflict           = "state-conflict"
	ErrTxInvalid               = "tx-invalid"
	ErrOutpointConflict        = "outpoint-conflict"
)

// Error is a coded contract error.
type Error struct {
	Code    string
	Message string
}

func (e *Error) Error() string {
	if e.Message == "" {
		return e.Code
	}
	return e.Code + ": " + e.Message
}

// CodeOf extracts the contract code from an error.
func CodeOf(err error) string {
	if e, ok := err.(*Error); ok {
		return e.Code
	}
	return fmt.Sprintf("unmapped:%v", err)
}

func fail(code, message string) *Error {
	return &Error{Code: code, Message: message}
}
