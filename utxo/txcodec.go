package utxo

import (
	"encoding/binary"
	"encoding/hex"
	"sort"
	"strconv"
	"strings"
)

// TxInput is one parsed transaction input.
type TxInput struct {
	TxID        string
	Vout        uint32
	ScriptSig   string
	Sequence    uint32
	OutpointKey string
}

// TxOutput is one parsed transaction output.
type TxOutput struct {
	Value  uint64
	Script string
}

// ParsedTx is a parsed transaction with its canonical txid.
type ParsedTx struct {
	Version       uint32
	Inputs        []TxInput
	Outputs       []TxOutput
	Locktime      uint32
	CanonicalTxID string
	SizeBytes     int
}

type txReader struct {
	data []byte
	pos  int
}

func (r *txReader) left() int { return len(r.data) - r.pos }

func (r *txReader) u8() (byte, error) {
	if r.left() < 1 {
		return 0, fail(ErrTxInvalid, "tx truncated")
	}
	value := r.data[r.pos]
	r.pos++
	return value, nil
}

func (r *txReader) u32le() (uint32, error) {
	if r.left() < 4 {
		return 0, fail(ErrTxInvalid, "tx truncated")
	}
	value := binary.LittleEndian.Uint32(r.data[r.pos:])
	r.pos += 4
	return value, nil
}

func (r *txReader) u64leMoney() (uint64, error) {
	if r.left() < 8 {
		return 0, fail(ErrTxInvalid, "tx truncated")
	}
	value := binary.LittleEndian.Uint64(r.data[r.pos:])
	r.pos += 8
	if value > MaxMoneySats {
		return 0, fail(ErrTxInvalid, "output value exceeds max money")
	}
	return value, nil
}

func (r *txReader) txid() (string, error) {
	if r.left() < 32 {
		return "", fail(ErrTxInvalid, "tx truncated")
	}
	le := r.data[r.pos : r.pos+32]
	r.pos += 32
	reversed := make([]byte, 32)
	for i := 0; i < 32; i++ {
		reversed[i] = le[31-i]
	}
	return hex.EncodeToString(reversed), nil
}

func (r *txReader) varint() (uint64, error) {
	prefix, err := r.u8()
	if err != nil {
		return 0, err
	}
	if prefix < 0xfd {
		return uint64(prefix), nil
	}
	if prefix == 0xfd {
		if r.left() < 2 {
			return 0, fail(ErrTxInvalid, "tx truncated")
		}
		value := uint64(r.data[r.pos]) | uint64(r.data[r.pos+1])<<8
		r.pos += 2
		if value < 0xfd {
			return 0, fail(ErrTxInvalid, "non-canonical varint")
		}
		return value, nil
	}
	if prefix == 0xfe {
		value, err := r.u32le()
		if err != nil {
			return 0, err
		}
		if value < 0x10000 {
			return 0, fail(ErrTxInvalid, "non-canonical varint")
		}
		return uint64(value), nil
	}
	lo, err := r.u32le()
	if err != nil {
		return 0, err
	}
	hi, err := r.u32le()
	if err != nil {
		return 0, err
	}
	if hi != 0 {
		return 0, fail(ErrTxInvalid, "varint too large")
	}
	return uint64(lo), nil
}

func (r *txReader) slice(n int) ([]byte, error) {
	if n < 0 || r.left() < n {
		return nil, fail(ErrTxInvalid, "tx truncated")
	}
	out := r.data[r.pos : r.pos+n]
	r.pos += n
	return out, nil
}

// EncodeVarint encodes a canonical CompactSize value.
func EncodeVarint(value uint64) ([]byte, error) {
	if value < 0xfd {
		return []byte{byte(value)}, nil
	}
	if value <= 0xffff {
		return []byte{0xfd, byte(value), byte(value >> 8)}, nil
	}
	if value <= 0xffffffff {
		out := make([]byte, 5)
		out[0] = 0xfe
		binary.LittleEndian.PutUint32(out[1:], uint32(value))
		return out, nil
	}
	return nil, fail(ErrTxInvalid, "varint too large")
}

// VarintSize returns the encoded length of a CompactSize value.
func VarintSize(value uint64) (int, error) {
	if value < 0xfd {
		return 1, nil
	}
	if value <= 0xffff {
		return 3, nil
	}
	if value <= 0xffffffff {
		return 5, nil
	}
	return 0, fail(ErrTxInvalid, "varint too large")
}

// ParseTx parses exact raw transaction bytes.
func ParseTx(rawTxHex string, expectedTxid string) (*ParsedTx, error) {
	raw, err := HexToBytes(rawTxHex)
	if err != nil {
		return nil, fail(ErrTxInvalid, "rawTx is not hex")
	}
	if len(raw) < 10 || len(raw) > 4000000 {
		return nil, fail(ErrTxInvalid, "tx size out of bounds")
	}
	r := &txReader{data: raw}
	version, err := r.u32le()
	if err != nil {
		return nil, err
	}
	nIn, err := r.varint()
	if err != nil {
		return nil, err
	}
	if nIn == 0 || nIn > 100000 {
		return nil, fail(ErrTxInvalid, "bad input count")
	}
	inputs := make([]TxInput, 0, nIn)
	seenIn := map[string]bool{}
	for i := uint64(0); i < nIn; i++ {
		txid, err := r.txid()
		if err != nil {
			return nil, err
		}
		vout, err := r.u32le()
		if err != nil {
			return nil, err
		}
		scriptLen, err := r.varint()
		if err != nil {
			return nil, err
		}
		if scriptLen > 10000 {
			return nil, fail(ErrTxInvalid, "scriptSig too long")
		}
		script, err := r.slice(int(scriptLen))
		if err != nil {
			return nil, err
		}
		sequence, err := r.u32le()
		if err != nil {
			return nil, err
		}
		key := txid + ":" + strconv.FormatUint(uint64(vout), 10)
		if seenIn[key] {
			return nil, fail(ErrTxInvalid, "duplicate input")
		}
		seenIn[key] = true
		inputs = append(inputs, TxInput{TxID: txid, Vout: vout, ScriptSig: hex.EncodeToString(script), Sequence: sequence, OutpointKey: key})
	}
	nOut, err := r.varint()
	if err != nil {
		return nil, err
	}
	if nOut == 0 || nOut > 100000 {
		return nil, fail(ErrTxInvalid, "bad output count")
	}
	outputs := make([]TxOutput, 0, nOut)
	for i := uint64(0); i < nOut; i++ {
		value, err := r.u64leMoney()
		if err != nil {
			return nil, err
		}
		scriptLen, err := r.varint()
		if err != nil {
			return nil, err
		}
		if scriptLen > 10000 {
			return nil, fail(ErrTxInvalid, "script too long")
		}
		script, err := r.slice(int(scriptLen))
		if err != nil {
			return nil, err
		}
		outputs = append(outputs, TxOutput{Value: value, Script: hex.EncodeToString(script)})
	}
	locktime, err := r.u32le()
	if err != nil {
		return nil, err
	}
	if r.left() != 0 {
		return nil, fail(ErrTxInvalid, "trailing bytes")
	}
	canonical := CalcTxid(raw)
	if expectedTxid != "" && strings.ToLower(expectedTxid) != canonical {
		return nil, fail(ErrTxInvalid, "txid does not match raw transaction")
	}
	return &ParsedTx{Version: version, Inputs: inputs, Outputs: outputs, Locktime: locktime, CanonicalTxID: canonical, SizeBytes: len(raw)}, nil
}

func uintToString(value uint64) string {
	return strconv.FormatUint(value, 10)
}

func u32leBytes(value uint32) []byte {
	out := make([]byte, 4)
	binary.LittleEndian.PutUint32(out, value)
	return out
}

func u64leBytes(value uint64) ([]byte, error) {
	if value > MaxMoneySats {
		return nil, fail(ErrAmountInvalid, "output value out of range")
	}
	out := make([]byte, 8)
	binary.LittleEndian.PutUint64(out, value)
	return out, nil
}

func txidToLEBytes(txid string) ([]byte, error) {
	if _, err := CheckTxid(txid); err != nil {
		return nil, err
	}
	be, err := hex.DecodeString(txid)
	if err != nil {
		return nil, fail(ErrTxInvalid, "invalid txid")
	}
	le := make([]byte, 32)
	for i := 0; i < 32; i++ {
		le[i] = be[31-i]
	}
	return le, nil
}

// UnsignedInput is one template input.
type UnsignedInput struct {
	TxID     string
	Vout     uint32
	Sequence uint32
	HasSeq   bool
}

// UnsignedOutput is one template output.
type UnsignedOutput struct {
	Value  uint64
	Script string
}

// BuildUnsigned builds an unsigned transaction template.
func BuildUnsigned(version uint32, locktime uint32, inputs []UnsignedInput, outputs []UnsignedOutput) (string, error) {
	var parts [][]byte
	parts = append(parts, u32leBytes(version))
	count, err := EncodeVarint(uint64(len(inputs)))
	if err != nil {
		return "", err
	}
	parts = append(parts, count)
	seen := map[string]bool{}
	for _, input := range inputs {
		key := input.TxID + ":" + uintToString(uint64(input.Vout))
		if seen[key] {
			return "", fail(ErrTxInvalid, "duplicate input")
		}
		seen[key] = true
		le, err := txidToLEBytes(input.TxID)
		if err != nil {
			return "", err
		}
		parts = append(parts, le, u32leBytes(input.Vout))
		zero, err := EncodeVarint(0)
		if err != nil {
			return "", err
		}
		parts = append(parts, zero)
		sequence := uint32(0xfffffffe)
		if input.HasSeq {
			sequence = input.Sequence
		}
		parts = append(parts, u32leBytes(sequence))
	}
	count, err = EncodeVarint(uint64(len(outputs)))
	if err != nil {
		return "", err
	}
	parts = append(parts, count)
	for _, output := range outputs {
		valueBytes, err := u64leBytes(output.Value)
		if err != nil {
			return "", err
		}
		script, err := HexToBytes(output.Script)
		if err != nil {
			return "", fail(ErrTxInvalid, "bad output script")
		}
		length, err := EncodeVarint(uint64(len(script)))
		if err != nil {
			return "", err
		}
		parts = append(parts, valueBytes, length, script)
	}
	parts = append(parts, u32leBytes(locktime))
	var total []byte
	for _, part := range parts {
		total = append(total, part...)
	}
	return hex.EncodeToString(total), nil
}

// EstimateSignedSize estimates the signed size: exact base + compact counts +
// P2PKH input upper-bound weight each + exact output bytes.
func EstimateSignedSize(nInputs int, outputScriptLengths []int) (uint64, error) {
	if nInputs <= 0 {
		return 0, fail(ErrPolicyDenied, "input count out of range")
	}
	inCount, err := VarintSize(uint64(nInputs))
	if err != nil {
		return 0, err
	}
	outCount, err := VarintSize(uint64(len(outputScriptLengths)))
	if err != nil {
		return 0, err
	}
	running := uint64(4 + inCount + nInputs*P2PKHInputWeight + outCount)
	for _, length := range outputScriptLengths {
		if length < 0 || length > 10000 {
			return 0, fail(ErrPolicyDenied, "script length out of range")
		}
		scriptCount, err := VarintSize(uint64(length))
		if err != nil {
			return 0, err
		}
		running += uint64(8 + scriptCount + length)
	}
	running += 4
	return running, nil
}

// AssembleSigned attaches unlocking scripts to an unsigned template,
// re-serializing canonically. Hosts use this to combine kernel-built
// templates with signatures from the trusted signer.
func AssembleSigned(unsignedTxHex string, scriptSigs []string) (string, error) {
	parsed, err := ParseTx(unsignedTxHex, "")
	if err != nil {
		return "", err
	}
	if len(scriptSigs) != len(parsed.Inputs) {
		return "", fail(ErrTxInvalid, "signature count mismatch")
	}
	var parts [][]byte
	parts = append(parts, u32leBytes(parsed.Version))
	count, err := EncodeVarint(uint64(len(parsed.Inputs)))
	if err != nil {
		return "", err
	}
	parts = append(parts, count)
	for i, input := range parsed.Inputs {
		script, err := HexToBytes(scriptSigs[i])
		if err != nil {
			return "", fail(ErrTxInvalid, "bad unlocking script")
		}
		if len(script) > 10000 {
			return "", fail(ErrTxInvalid, "unlocking script too long")
		}
		le, err := txidToLEBytes(input.TxID)
		if err != nil {
			return "", err
		}
		length, err := EncodeVarint(uint64(len(script)))
		if err != nil {
			return "", err
		}
		parts = append(parts, le, u32leBytes(input.Vout), length, script, u32leBytes(input.Sequence))
	}
	count, err = EncodeVarint(uint64(len(parsed.Outputs)))
	if err != nil {
		return "", err
	}
	parts = append(parts, count)
	for _, output := range parsed.Outputs {
		valueBytes, err := u64leBytes(output.Value)
		if err != nil {
			return "", err
		}
		script, err := HexToBytes(output.Script)
		if err != nil {
			return "", fail(ErrTxInvalid, "bad output script")
		}
		length, err := EncodeVarint(uint64(len(script)))
		if err != nil {
			return "", err
		}
		parts = append(parts, valueBytes, length, script)
	}
	parts = append(parts, u32leBytes(parsed.Locktime))
	var total []byte
	for _, part := range parts {
		total = append(total, part...)
	}
	return hex.EncodeToString(total), nil
}

// SortedOutpointKeys sorts outpoint keys deterministically.
func SortedOutpointKeys(keys []string) []string {
	out := append([]string{}, keys...)
	sort.Strings(out)
	return out
}
