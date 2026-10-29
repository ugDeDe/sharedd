package blueprint

import (
	"slices"
	"testing"
)

func TestBlocksCoverSpecTable(t *testing.T) {
	want := []string{
		"event.ip_ban", "install.text", "ip.attach", "ip.cleanup", "ip.order",
		"ip.switch_advertised", "ip.verify", "observe.arm", "policy.guard",
		"pool.admit", "resource.fence", "resource.switch", "server.absent",
		"server.create", "server.delete", "server.distinct_ip", "server.ready",
		"terminal.stop",
	}
	got := Blocks()
	if len(got) != len(want) {
		t.Fatalf("block count = %d, want %d", len(got), len(want))
	}
	for i, b := range got {
		if b.Type != want[i] {
			t.Fatalf("blocks[%d] = %s, want %s (must be sorted)", i, b.Type, want[i])
		}
	}
}

func TestControlPortsAndRetry(t *testing.T) {
	cases := []struct {
		typ     string
		ctrl    []string
		retry   string
		effects string
	}{
		{"policy.guard", []string{"active", "stop"}, RetryNone, EffectRead},
		{"ip.order", []string{"ok", "fallback"}, RetryNone, EffectPurchase},
		{"ip.attach", []string{"ok", "fallback"}, RetryNone, EffectMutation},
		{"ip.switch_advertised", []string{"ok", "fallback"}, RetryNone, EffectMutation},
		{"server.ready", []string{"ok"}, RetrySafeRead, EffectRead},
		{"install.text", []string{"ok"}, RetryNone, EffectMutation},
		{"observe.arm", nil, RetryNone, EffectMutation},
		{"terminal.stop", nil, RetryNone, EffectControl},
		{"server.create", []string{"ok"}, RetryNone, EffectPurchase},
	}
	for _, tc := range cases {
		b, ok := BlockFor(tc.typ)
		if !ok {
			t.Fatalf("missing block %s", tc.typ)
		}
		if len(b.Ports.Control) != len(tc.ctrl) || !slices.Equal(b.Ports.Control, tc.ctrl) {
			t.Fatalf("%s control = %v, want %v", tc.typ, b.Ports.Control, tc.ctrl)
		}
		if b.RetryClass != tc.retry {
			t.Fatalf("%s retry = %s, want %s", tc.typ, b.RetryClass, tc.retry)
		}
		if b.Effects != tc.effects {
			t.Fatalf("%s effects = %s, want %s", tc.typ, b.Effects, tc.effects)
		}
	}
}

func TestProviderCapabilities(t *testing.T) {
	for _, begetOnly := range []string{"ip.order", "ip.attach", "ip.switch_advertised", "ip.cleanup", "ip.verify"} {
		b, _ := BlockFor(begetOnly)
		if slices.Contains(b.Capabilities, ProviderNodehost) || !slices.Contains(b.Capabilities, ProviderBeget) {
			t.Fatalf("%s must be beget-only, got %v", begetOnly, b.Capabilities)
		}
	}
	// server.distinct_ip is used by both templates, so it is valid for both providers.
	b, _ := BlockFor("server.distinct_ip")
	if !slices.Contains(b.Capabilities, ProviderNodehost) || !slices.Contains(b.Capabilities, ProviderBeget) {
		t.Fatalf("server.distinct_ip must be available for both providers, got %v", b.Capabilities)
	}
}

func TestTimeoutParams(t *testing.T) {
	for _, typ := range []string{"server.ready", "install.text", "pool.admit", "server.absent", "ip.switch_advertised"} {
		b, _ := BlockFor(typ)
		if len(b.ParamsSchema) != 1 || b.ParamsSchema[0].Name != "timeout_seconds" || !b.ParamsSchema[0].Required {
			t.Fatalf("%s must require timeout_seconds, got %+v", typ, b.ParamsSchema)
		}
	}
}
