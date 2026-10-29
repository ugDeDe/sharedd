package agent

import "testing"

func TestValidAdTag(t *testing.T) {
	for _, ok := range []string{
		"0123456789abcdef0123456789abcdef",
		"AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA",
	} {
		if !validAdTag(ok) {
			t.Errorf("validAdTag(%q) = false", ok)
		}
	}
	for _, bad := range []string{"", "xyz", "0123", "0123456789abcdef0123456789abcdeg"} {
		if validAdTag(bad) {
			t.Errorf("validAdTag(%q) = true", bad)
		}
	}
}

func TestApplyNodeSettingsUnmanagedIsNoop(t *testing.T) {
	// Must not touch the proxy or report anything.
	applyNodeSettings(nil, &NodeSettings{Revision: 1, AdTag: AdTagSetting{Mode: "unmanaged"}})
	applyNodeSettings(nil, nil)
}
