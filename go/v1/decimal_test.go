package v1

import (
	"errors"
	"strings"
	"testing"
)

func TestCheckDecimal(t *testing.T) {
	tests := []struct {
		name    string
		value   string
		wantErr error
	}{
		{"whole number", "1000", nil},
		{"two decimals", "1000.25", nil},
		{"exactly six decimals", "1000.123456", nil},
		{"padding is not precision", "1000.1000000", nil},
		{"negative within scale", "-10.85", nil},
		{"seventh decimal", "1000.1234567", ErrDecimalPrecision},
		{"seventh decimal is a zero run", "1000.0000001", ErrDecimalPrecision},
		{"empty", "", errParse},
		{"not a number", "abc", errParse},
		{"comma instead of point", "1,5", errParse},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := checkDecimal("quantity", tt.value)
			switch {
			case tt.wantErr == nil:
				if err != nil {
					t.Fatalf("checkDecimal(%q) = %v, want nil", tt.value, err)
				}
			case errors.Is(tt.wantErr, errParse):
				if err == nil || errors.Is(err, ErrDecimalPrecision) {
					t.Fatalf("checkDecimal(%q) = %v, want a parse failure", tt.value, err)
				}
			default:
				if !errors.Is(err, tt.wantErr) {
					t.Fatalf("checkDecimal(%q) = %v, want %v", tt.value, err, tt.wantErr)
				}
			}
			// The field name has to reach the caller: three decimal fields share
			// this check and the message is all that tells them apart.
			if err != nil && !strings.Contains(err.Error(), "quantity") {
				t.Errorf("error %q does not name the field", err)
			}
		})
	}
}

// errParse is a marker for the table above: the parse failure comes from
// udecimal and carries no sentinel of its own.
var errParse = errors.New("parse")
