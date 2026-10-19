package nodehost

import (
	"encoding/json"
	"errors"
	"regexp"
	"strconv"
	"strings"
)

// Decimal is an exact, bounded decimal. JSON output is always a string.
// Its zero value is absent, not a monetary zero.
type Decimal string

var decimalPattern = regexp.MustCompile(`^-?(0|[1-9][0-9]*)(\.[0-9]+)?([eE][+-]?[0-9]+)?$`)

func (d *Decimal) UnmarshalJSON(data []byte) error {
	s := string(data)
	if len(data) > 260 {
		return errors.New("invalid decimal")
	}
	if len(data) > 0 && data[0] == '"' {
		if json.Unmarshal(data, &s) != nil {
			return errors.New("invalid decimal")
		}
	}
	if len(s) > 128 || !decimalPattern.MatchString(s) {
		return errors.New("invalid decimal")
	}
	// Validate JSON numeric grammar without ever converting through float64.
	var n json.Number
	if json.Unmarshal([]byte(s), &n) != nil {
		return errors.New("invalid decimal")
	}
	parts := strings.FieldsFunc(n.String(), func(r rune) bool { return r == 'e' || r == 'E' })
	if len(parts) == 1 {
		*d = Decimal(s)
		return nil
	}
	exp, err := strconv.Atoi(parts[1])
	if err != nil || exp < -128 || exp > 128 {
		return errors.New("invalid decimal")
	}
	sign, mantissa := "", parts[0]
	if strings.HasPrefix(mantissa, "-") {
		sign, mantissa = "-", mantissa[1:]
	}
	point := strings.IndexByte(mantissa, '.')
	if point < 0 {
		point = len(mantissa)
	}
	digits := strings.ReplaceAll(mantissa, ".", "")
	point += exp
	switch {
	case point <= 0:
		s = "0." + strings.Repeat("0", -point) + digits
	case point >= len(digits):
		s = digits + strings.Repeat("0", point-len(digits))
	default:
		s = digits[:point] + "." + digits[point:]
	}
	*d = Decimal(sign + s)
	return nil
}
