package utxo

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"math/big"
	"math/bits"
	"regexp"
)

// Connect v1 payment-template oracle. This file deliberately mirrors
// keymaster.cc/packages/utxo-manager/src/paymentTemplate.ts. It does not select coins, sign, or grant
// authorization; it only freezes and re-checks the supported output roles.

type PaymentType string
type PaymentOutputRole string
type PaymentScriptKind string

const (
	PaymentTypeP2PKH PaymentType = "p2pkh"
	PaymentTypeP2PK  PaymentType = "p2pk"

	PaymentRoleBusiness PaymentOutputRole = "business"
	PaymentRoleChange   PaymentOutputRole = "change"

	PaymentScriptP2PKH    PaymentScriptKind = "p2pkh"
	PaymentScriptP2PK     PaymentScriptKind = "p2pk"
	PaymentScriptOpReturn PaymentScriptKind = "op-return"
	PaymentScriptUnknown  PaymentScriptKind = "unknown"
)

type PaymentOutputFact struct {
	Vout      int    `json:"vout"`
	ScriptHex string `json:"scriptHex"`
	ValueSats string `json:"valueSats"`
}

type PaymentRoleFact struct {
	Vout      int               `json:"vout"`
	ScriptHex string            `json:"scriptHex"`
	ValueSats string            `json:"valueSats"`
	Role      PaymentOutputRole `json:"role"`
}

type PaymentPrevoutFact struct {
	ScriptHex string
	ValueSats string
}

type PaymentTemplatePlan struct {
	PaymentType        PaymentType
	BusinessAmountSats string
	BusinessScriptHex  string
	ChangeScriptHex    string
	ChangeAmountSats   string
}

type PaymentTemplateValidation struct {
	BusinessSats string `json:"businessSats"`
	ChangeSats   string `json:"changeSats"`
	FeeSats      string `json:"feeSats"`
	TotalInSats  string `json:"totalInSats"`
}

var paymentHexPattern = regexp.MustCompile(`^[0-9a-fA-F]+$`)

func normalizePaymentHex(value, field string, exactBytes int) (string, error) {
	if value == "" || len(value)%2 != 0 || !paymentHexPattern.MatchString(value) {
		return "", fail(ErrPolicyDenied, field+" must be hex")
	}
	if exactBytes >= 0 && len(value) != exactBytes*2 {
		return "", fail(ErrPolicyDenied, field+" has the wrong length")
	}
	return hex.EncodeToString(mustHexDecode(value)), nil
}

func mustHexDecode(value string) []byte {
	decoded, err := hex.DecodeString(value)
	if err != nil {
		panic(err)
	}
	return decoded
}

func readPaymentPush(script []byte, offset int) (opcode int, next int, ok bool) {
	if offset >= len(script) {
		return 0, 0, false
	}
	opcode = int(script[offset])
	if opcode <= 0x4b {
		return opcode, offset + 1 + opcode, offset+1+opcode <= len(script)
	}
	switch opcode {
	case 0x4c:
		if offset+2 > len(script) {
			return 0, 0, false
		}
		length := int(script[offset+1])
		return opcode, offset + 2 + length, offset+2+length <= len(script)
	case 0x4d:
		if offset+3 > len(script) {
			return 0, 0, false
		}
		length := int(script[offset+1]) | int(script[offset+2])<<8
		return opcode, offset + 3 + length, offset+3+length <= len(script)
	case 0x4e:
		if offset+5 > len(script) {
			return 0, 0, false
		}
		length := int(script[offset+1]) | int(script[offset+2])<<8 | int(script[offset+3])<<16 | int(script[offset+4])<<24
		return opcode, offset + 5 + length, offset+5+length <= len(script)
	}
	return opcode, offset + 1, true
}

// ClassifyPaymentScript parses opcodes; pushed data containing 0x6a does not
// turn a legal template into an OP_RETURN output.
func ClassifyPaymentScript(scriptHex string) (PaymentScriptKind, error) {
	normalized, err := normalizePaymentHex(scriptHex, "scriptHex", -1)
	if err != nil {
		return "", err
	}
	script := mustHexDecode(normalized)
	if len(script) == 25 && script[0] == 0x76 && script[1] == 0xa9 && script[2] == 0x14 &&
		script[23] == 0x88 && script[24] == 0xac {
		return PaymentScriptP2PKH, nil
	}
	if len(script) == 35 && script[0] == 0x21 && script[34] == 0xac && IsValidCompressedPublicKey(normalized[2:68]) {
		return PaymentScriptP2PK, nil
	}
	offset := 0
	previousWasFalse := false
	for offset < len(script) {
		opcode, next, ok := readPaymentPush(script, offset)
		if !ok {
			return PaymentScriptUnknown, nil
		}
		if opcode == 0x6a || (previousWasFalse && opcode == 0x6a) {
			return PaymentScriptOpReturn, nil
		}
		previousWasFalse = opcode == 0x00
		offset = next
	}
	return PaymentScriptUnknown, nil
}

var secp256k1P, _ = new(big.Int).SetString("fffffffffffffffffffffffffffffffffffffffffffffffffffffffefffffc2f", 16)

// IsValidCompressedPublicKey checks a compressed secp256k1 point, not merely
// the 33-byte prefix/length.
func IsValidCompressedPublicKey(publicKeyHex string) bool {
	normalized, err := normalizePaymentHex(publicKeyHex, "publicKeyHex", 33)
	if err != nil || (normalized[:2] != "02" && normalized[:2] != "03") {
		return false
	}
	x, ok := new(big.Int).SetString(normalized[2:], 16)
	if !ok || x.Cmp(secp256k1P) >= 0 {
		return false
	}
	rhs := new(big.Int).Mul(x, x)
	rhs.Mul(rhs, x)
	rhs.Add(rhs, big.NewInt(7))
	rhs.Mod(rhs, secp256k1P)
	exponent := new(big.Int).Add(secp256k1P, big.NewInt(1))
	exponent.Rsh(exponent, 2)
	y := new(big.Int).Exp(rhs, exponent, secp256k1P)
	check := new(big.Int).Mul(y, y)
	check.Mod(check, secp256k1P)
	if check.Cmp(rhs) != 0 {
		return false
	}
	wantOdd := normalized[:2] == "03"
	if (y.Bit(0) == 1) != wantOdd {
		y.Sub(secp256k1P, y)
	}
	return true
}

func P2PKHScriptFromHash160(hash160Hex string) (string, error) {
	hash, err := normalizePaymentHex(hash160Hex, "hash160Hex", 20)
	if err != nil {
		return "", err
	}
	return "76a914" + hash + "88ac", nil
}

func P2PKHScriptFromPublicKey(publicKeyHex string) (string, error) {
	key, err := normalizePaymentHex(publicKeyHex, "publicKeyHex", 33)
	if err != nil {
		return "", err
	}
	if !IsValidCompressedPublicKey(key) {
		return "", fail(ErrPolicyDenied, "P2PKH public key is not a valid compressed secp256k1 point")
	}
	digest, err := Hash160Hex(key)
	if err != nil {
		return "", err
	}
	return P2PKHScriptFromHash160(digest)
}

func P2PKScriptFromPublicKey(publicKeyHex string) (string, error) {
	key, err := normalizePaymentHex(publicKeyHex, "publicKeyHex", 33)
	if err != nil {
		return "", err
	}
	if !IsValidCompressedPublicKey(key) {
		return "", fail(ErrPolicyDenied, "P2PK public key is not a valid compressed secp256k1 point")
	}
	return "21" + key + "ac", nil
}

func PaymentScriptMaterial(scriptHex string) (kind PaymentScriptKind, hash160Hex, publicKeyHex string, err error) {
	kind, err = ClassifyPaymentScript(scriptHex)
	if err != nil {
		return "", "", "", err
	}
	normalized, _ := normalizePaymentHex(scriptHex, "scriptHex", -1)
	if kind == PaymentScriptP2PKH {
		return kind, normalized[6:46], "", nil
	}
	if kind == PaymentScriptP2PK {
		return kind, "", normalized[2:68], nil
	}
	return kind, "", "", nil
}

func PublisherMatchesPaymentScript(scriptHex, publisherPublicKeyHex string) (bool, error) {
	kind, hash160Hex, publicKeyHex, err := PaymentScriptMaterial(scriptHex)
	if err != nil {
		return false, err
	}
	publisher, err := normalizePaymentHex(publisherPublicKeyHex, "publisherPublicKeyHex", 33)
	if err != nil {
		return false, err
	}
	if !IsValidCompressedPublicKey(publisher) {
		return false, fail(ErrPolicyDenied, "publisherPublicKeyHex is not a valid compressed secp256k1 point")
	}
	switch kind {
	case PaymentScriptP2PKH:
		digest, err := Hash160Hex(publisher)
		if err != nil {
			return false, err
		}
		return digest == hash160Hex, nil
	case PaymentScriptP2PK:
		return publisher == publicKeyHex, nil
	default:
		return false, nil
	}
}

// BuildPaymentTemplate freezes vout+script+value+role. vout 0 is the business
// output; optional vout 1 is wallet P2PKH change.
func BuildPaymentTemplate(plan PaymentTemplatePlan) ([]PaymentRoleFact, error) {
	if plan.PaymentType != PaymentTypeP2PKH && plan.PaymentType != PaymentTypeP2PK {
		return nil, fail(ErrPolicyDenied, "unsupported payment type")
	}
	businessAmount, err := ParseAmount(plan.BusinessAmountSats, "businessAmountSats")
	if err != nil {
		return nil, err
	}
	if businessAmount == 0 {
		return nil, fail(ErrPolicyDenied, "business amount must be positive")
	}
	businessScript, err := normalizePaymentHex(plan.BusinessScriptHex, "businessScriptHex", -1)
	if err != nil {
		return nil, err
	}
	businessKind, err := ClassifyPaymentScript(businessScript)
	if err != nil {
		return nil, err
	}
	if string(businessKind) != string(plan.PaymentType) {
		return nil, fail(ErrPolicyDenied, "business output does not match payment type")
	}
	outputs := []PaymentRoleFact{{
		Vout: 0, ScriptHex: businessScript, ValueSats: plan.BusinessAmountSats, Role: PaymentRoleBusiness,
	}}
	changeAmount := uint64(0)
	if plan.ChangeAmountSats != "" {
		changeAmount, err = ParseAmount(plan.ChangeAmountSats, "changeAmountSats")
		if err != nil {
			return nil, err
		}
	}
	if changeAmount > 0 {
		if plan.ChangeScriptHex == "" {
			return nil, fail(ErrPolicyDenied, "positive change requires a wallet P2PKH script")
		}
		changeScript, err := normalizePaymentHex(plan.ChangeScriptHex, "changeScriptHex", -1)
		if err != nil {
			return nil, err
		}
		changeKind, err := ClassifyPaymentScript(changeScript)
		if err != nil {
			return nil, err
		}
		if changeKind != PaymentScriptP2PKH {
			return nil, fail(ErrPolicyDenied, "change must be an exact P2PKH script")
		}
		outputs = append(outputs, PaymentRoleFact{
			Vout: 1, ScriptHex: changeScript, ValueSats: plan.ChangeAmountSats, Role: PaymentRoleChange,
		})
	}
	return outputs, nil
}

// ValidatePaymentTemplate independently re-checks real prevout scripts and
// output facts against the frozen plan.
func ValidatePaymentTemplate(plan PaymentTemplatePlan, prevouts []PaymentPrevoutFact, outputs []PaymentOutputFact) (PaymentTemplateValidation, error) {
	if len(prevouts) == 0 {
		return PaymentTemplateValidation{}, fail(ErrPolicyDenied, "payment requires at least one input")
	}
	var totalIn uint64
	for index, prevout := range prevouts {
		kind, err := ClassifyPaymentScript(prevout.ScriptHex)
		if err != nil {
			return PaymentTemplateValidation{}, err
		}
		if kind != PaymentScriptP2PKH {
			return PaymentTemplateValidation{}, fail(ErrPolicyDenied, "input is not an exact P2PKH output")
		}
		value, err := ParseAmount(prevout.ValueSats, "prevout valueSats")
		if err != nil {
			return PaymentTemplateValidation{}, err
		}
		totalIn, err = AddChecked(totalIn, value, "payment inputs")
		if err != nil {
			return PaymentTemplateValidation{}, err
		}
		_ = index
	}
	expected, err := BuildPaymentTemplate(plan)
	if err != nil {
		return PaymentTemplateValidation{}, err
	}
	if len(outputs) != len(expected) {
		return PaymentTemplateValidation{}, fail(ErrPolicyDenied, "payment output count does not match the frozen template")
	}
	seen := map[int]bool{}
	var business, change uint64
	for _, actual := range outputs {
		if actual.Vout < 0 || seen[actual.Vout] {
			return PaymentTemplateValidation{}, fail(ErrTxInvalid, "invalid or duplicate output vout")
		}
		seen[actual.Vout] = true
		var row *PaymentRoleFact
		for i := range expected {
			if expected[i].Vout == actual.Vout {
				row = &expected[i]
				break
			}
		}
		if row == nil {
			return PaymentTemplateValidation{}, fail(ErrPolicyDenied, "unexpected output vout")
		}
		actualScript, err := normalizePaymentHex(actual.ScriptHex, "output scriptHex", -1)
		if err != nil {
			return PaymentTemplateValidation{}, err
		}
		if actualScript != row.ScriptHex {
			return PaymentTemplateValidation{}, fail(ErrPolicyDenied, "output script does not match its frozen role")
		}
		value, err := ParseAmount(actual.ValueSats, "output valueSats")
		if err != nil {
			return PaymentTemplateValidation{}, err
		}
		expectedValue, _ := ParseAmount(row.ValueSats, "template valueSats")
		if value != expectedValue {
			return PaymentTemplateValidation{}, fail(ErrPolicyDenied, "output value does not match its frozen role")
		}
		if row.Role == PaymentRoleBusiness {
			business, err = AddChecked(business, value, "business outputs")
		} else {
			change, err = AddChecked(change, value, "change outputs")
		}
		if err != nil {
			return PaymentTemplateValidation{}, err
		}
	}
	totalOut, err := AddChecked(business, change, "payment outputs")
	if err != nil {
		return PaymentTemplateValidation{}, err
	}
	if totalOut > totalIn {
		return PaymentTemplateValidation{}, fail(ErrPolicyDenied, "outputs exceed inputs")
	}
	return PaymentTemplateValidation{
		BusinessSats: mustFormat(business), ChangeSats: mustFormat(change),
		FeeSats: mustFormat(totalIn - totalOut), TotalInSats: mustFormat(totalIn),
	}, nil
}

/* ---------------- RIPEMD-160 (for HASH160) ---------------- */

var rmdR = [80]int{
	0, 1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15,
	7, 4, 13, 1, 10, 6, 15, 3, 12, 0, 9, 5, 2, 14, 11, 8,
	3, 10, 14, 4, 9, 15, 8, 1, 2, 7, 0, 6, 13, 11, 5, 12,
	1, 9, 11, 10, 0, 8, 12, 4, 13, 3, 7, 15, 14, 5, 6, 2,
	4, 0, 5, 9, 7, 12, 2, 10, 14, 1, 3, 8, 11, 6, 15, 13,
}
var rmdRP = [80]int{
	5, 14, 7, 0, 9, 2, 11, 4, 13, 6, 15, 8, 1, 10, 3, 12,
	6, 11, 3, 7, 0, 13, 5, 10, 14, 15, 8, 12, 4, 9, 1, 2,
	15, 5, 1, 3, 7, 14, 6, 9, 11, 8, 12, 2, 10, 0, 4, 13,
	8, 6, 4, 1, 3, 11, 15, 0, 5, 12, 2, 13, 9, 7, 10, 14,
	12, 15, 10, 4, 1, 5, 8, 7, 6, 2, 13, 14, 0, 3, 9, 11,
}
var rmdS = [80]int{
	11, 14, 15, 12, 5, 8, 7, 9, 11, 13, 14, 15, 6, 7, 9, 8,
	7, 6, 8, 13, 11, 9, 7, 15, 7, 12, 15, 9, 11, 7, 13, 12,
	11, 13, 6, 7, 14, 9, 13, 15, 14, 8, 13, 6, 5, 12, 7, 5,
	11, 12, 14, 15, 14, 15, 9, 8, 9, 14, 5, 6, 8, 6, 5, 12,
	9, 15, 5, 11, 6, 8, 13, 12, 5, 12, 13, 14, 11, 8, 5, 6,
}
var rmdSP = [80]int{
	8, 9, 9, 11, 13, 15, 15, 5, 7, 7, 8, 11, 14, 14, 12, 6,
	9, 13, 15, 7, 12, 8, 9, 11, 7, 7, 12, 7, 6, 15, 13, 11,
	9, 7, 15, 11, 8, 6, 6, 14, 12, 13, 5, 14, 13, 13, 7, 5,
	15, 5, 8, 11, 14, 14, 6, 14, 6, 9, 12, 9, 12, 5, 15, 8,
	8, 5, 12, 9, 12, 5, 14, 6, 8, 13, 6, 5, 15, 13, 11, 11,
}
var rmdK = [5]uint32{0x00000000, 0x5a827999, 0x6ed9eba1, 0x8f1bbcdc, 0xa953fd4e}
var rmdKP = [5]uint32{0x50a28be6, 0x5c4dd124, 0x6d703ef3, 0x7a6d76e9, 0x00000000}

func rotl32(x uint32, n int) uint32 { return bits.RotateLeft32(x, n) }

func rmdF(round int, x, y, z uint32) uint32 {
	switch round {
	case 0:
		return x ^ y ^ z
	case 1:
		return (x & y) | (^x & z)
	case 2:
		return (x | ^y) ^ z
	case 3:
		return (x & z) | (y & ^z)
	default:
		return x ^ (y | ^z)
	}
}

// RIPEMD160 computes the RIPEMD-160 digest.
func RIPEMD160(data []byte) []byte {
	bitLen := uint64(len(data)) * 8
	paddedLen := ((len(data) + 8) >> 6 << 6) + 64
	padded := make([]byte, paddedLen)
	copy(padded, data)
	padded[len(data)] = 0x80
	binary.LittleEndian.PutUint64(padded[paddedLen-8:], bitLen)
	h0, h1, h2, h3, h4 := uint32(0x67452301), uint32(0xefcdab89), uint32(0x98badcfe), uint32(0x10325476), uint32(0xc3d2e1f0)
	var x [16]uint32
	for off := 0; off < paddedLen; off += 64 {
		for i := 0; i < 16; i++ {
			x[i] = binary.LittleEndian.Uint32(padded[off+i*4:])
		}
		al, bl, cl, dl, el := h0, h1, h2, h3, h4
		ar, br, cr, dr, er := h0, h1, h2, h3, h4
		for j := 0; j < 80; j++ {
			round := j / 16
			t := rotl32(al+rmdF(round, bl, cl, dl)+x[rmdR[j]]+rmdK[round], rmdS[j]) + el
			al, el, dl, cl, bl = el, dl, rotl32(cl, 10), bl, t
			t = rotl32(ar+rmdF(4-round, br, cr, dr)+x[rmdRP[j]]+rmdKP[round], rmdSP[j]) + er
			ar, er, dr, cr, br = er, dr, rotl32(cr, 10), br, t
		}
		t := h1 + cl + dr
		h1 = h2 + dl + er
		h2 = h3 + el + ar
		h3 = h4 + al + br
		h4 = h0 + bl + cr
		h0 = t
	}
	out := make([]byte, 20)
	binary.LittleEndian.PutUint32(out[0:], h0)
	binary.LittleEndian.PutUint32(out[4:], h1)
	binary.LittleEndian.PutUint32(out[8:], h2)
	binary.LittleEndian.PutUint32(out[12:], h3)
	binary.LittleEndian.PutUint32(out[16:], h4)
	return out
}

// Hash160 returns RIPEMD-160(SHA-256(data)) as lowercase hex.
func Hash160(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(RIPEMD160(sum[:]))
}

// Hash160Hex returns RIPEMD-160(SHA-256(data)) as lowercase hex.
func Hash160Hex(hexData string) (string, error) {
	bytesValue, err := HexToBytes(hexData)
	if err != nil {
		return "", fail(ErrPolicyDenied, "invalid public key hex")
	}
	return Hash160(bytesValue), nil
}
