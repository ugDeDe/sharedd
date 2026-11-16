package machineapi

import "testing"

func TestValidateNodeType(t *testing.T) {
	for _, nt := range []string{"classic", "mtproxyl", "mtproxyl_mtdd", "meko", "mtdd"} {
		if err := ValidateNodeType(nt); err != nil {
			t.Errorf("%q must be valid: %v", nt, err)
		}
	}
	for _, nt := range []string{"", "MTProxyL", "mtproxyl-mtdd", "telemt"} {
		if err := ValidateNodeType(nt); err == nil {
			t.Errorf("%q must be rejected", nt)
		}
	}
}
