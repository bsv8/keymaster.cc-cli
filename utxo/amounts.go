package utxo

import (
	"math/bits"
	"regexp"
	"strconv"
)

var canonicalAmount = regexp.MustCompile(`^(0|[1-9][0-9]*)$`)

// ParseAmount validates a canonical decimal amount string.
func ParseAmount(value string, context ...string) (uint64, error) {
	what := "amount"
	if len(context) > 0 {
		what = context[0]
	}
	if !canonicalAmount.MatchString(value) {
		return 0, fail(ErrAmountInvalid, what+" is not a canonical amount string")
	}
	parsed, err := strconv.ParseUint(value, 10, 64)
	if err != nil {
		return 0, fail(ErrAmountInvalid, what+" is not a canonical amount string")
	}
	if parsed > MaxMoneySats {
		return 0, fail(ErrAmountInvalid, what+" exceeds max money")
	}
	return parsed, nil
}

// FormatAmount renders a checked amount.
func FormatAmount(value uint64) (string, error) {
	if value > MaxMoneySats {
		return "", fail(ErrAmountInvalid, "amount out of range")
	}
	return strconv.FormatUint(value, 10), nil
}

func mustFormat(value uint64) string {
	formatted, err := FormatAmount(value)
	if err != nil {
		panic(err)
	}
	return formatted
}

// AddChecked adds with overflow protection against max money.
func AddChecked(a, b uint64, context ...string) (uint64, error) {
	what := "sum"
	if len(context) > 0 {
		what = context[0]
	}
	sum, carry := bits.Add64(a, b, 0)
	if carry != 0 || sum > MaxMoneySats {
		return 0, fail(ErrAmountInvalid, what+" overflows max money")
	}
	return sum, nil
}

// SumChecked sums with overflow protection.
func SumChecked(values []uint64, context ...string) (uint64, error) {
	what := "sum"
	if len(context) > 0 {
		what = context[0]
	}
	var total uint64
	for _, value := range values {
		next, err := AddChecked(total, value, what)
		if err != nil {
			return 0, err
		}
		total = next
	}
	return total, nil
}

// EstimateFeeSats computes ceil(rate*size/1000), minimum 1.
func EstimateFeeSats(feeRateSatPerKb, sizeBytes uint64) (uint64, error) {
	if feeRateSatPerKb == 0 {
		return 0, fail(ErrFeeInvalid, "fee rate must be positive")
	}
	hi, lo := bits.Mul64(feeRateSatPerKb, sizeBytes)
	if hi != 0 {
		return 0, fail(ErrFeeInvalid, "fee exceeds max money")
	}
	lo += 999
	if lo < 999 {
		return 0, fail(ErrFeeInvalid, "fee exceeds max money")
	}
	fee := lo / 1000
	if fee == 0 {
		fee = 1
	}
	if fee > MaxMoneySats {
		return 0, fail(ErrFeeInvalid, "fee exceeds max money")
	}
	return fee, nil
}

// ParseFeeRate validates a positive fee rate.
func ParseFeeRate(value string) (uint64, error) {
	rate, err := ParseAmount(value, "fee rate")
	if err != nil {
		return 0, err
	}
	if rate == 0 {
		return 0, fail(ErrFeeInvalid, "fee rate must be positive")
	}
	return rate, nil
}
