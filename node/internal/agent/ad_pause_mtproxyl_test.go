package agent

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"sharedd/node/internal/config"
)

// fakeMtproxyl — MTProxyL CLI: `secret adtag` пишет [access.user_ad_tags] в
// рабочий config.toml, как generate_telemt_config + SIGHUP.
type fakeMtproxyl struct {
	t     *testing.T
	path  string
	me    bool
	tags  map[string]string
	calls []string
	fail  error
}

func (f *fakeMtproxyl) write() {
	var b strings.Builder
	fmt.Fprintf(&b, "[general]\nuse_middle_proxy = %t\nad_tag = \"ffeeddccbbaa99887766554433221100\"\n\n[access.users]\nshared = \"0123456789abcdef0123456789abcdef\"\nowner = \"fedcba9876543210fedcba9876543210\"\n", f.me)
	if len(f.tags) > 0 {
		b.WriteString("\n[access.user_ad_tags]\n")
		keys := make([]string, 0, len(f.tags))
		for k := range f.tags {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			fmt.Fprintf(&b, "%s = %q\n", k, f.tags[k])
		}
	}
	if err := os.WriteFile(f.path, []byte(b.String()), 0o644); err != nil {
		f.t.Fatal(err)
	}
}

func newFakeMtproxyl(t *testing.T, users ...string) (*fakeMtproxyl, *config.NodeConfig) {
	t.Helper()
	dir := t.TempDir()
	f := &fakeMtproxyl{t: t, path: filepath.Join(dir, "config.toml"), me: true, tags: map[string]string{}}
	f.write()
	oldOn, oldUsers, oldCmd, oldPrev := adPauseOnMtproxyl, adPauseSharedUsers, applyMtproxylCmd, adPausePrevFile
	t.Cleanup(func() {
		adPauseOnMtproxyl, adPauseSharedUsers, applyMtproxylCmd, adPausePrevFile = oldOn, oldUsers, oldCmd, oldPrev
	})
	adPauseOnMtproxyl = func() bool { return true }
	adPauseSharedUsers = func() []string { return users }
	adPausePrevFile = filepath.Join(dir, "ad_pause_prev.json")
	applyMtproxylCmd = func(args ...string) error {
		f.calls = append(f.calls, strings.Join(args, " "))
		if f.fail != nil {
			return f.fail
		}
		if len(args) != 4 || args[0] != "secret" || args[1] != "adtag" {
			t.Fatalf("unexpected mtproxyl call: %v", args)
		}
		if args[3] == "remove" {
			delete(f.tags, args[2])
		} else {
			f.tags[args[2]] = args[3]
		}
		f.write()
		return nil
	}
	cfg := &config.NodeConfig{}
	cfg.Telemt.ConfigPath = f.path
	return f, cfg
}

func TestAdPauseMtproxylTouchesOnlySharedUsers(t *testing.T) {
	f, cfg := newFakeMtproxyl(t, "shared")
	f.tags["owner"] = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa" // личная метка чужого пользователя
	f.write()

	if got := applyAdPause(cfg, adPauseDirective{Active: true, Tag: strings.ToUpper(testPoolAdTag)}); got != adPausePool {
		t.Fatalf("set pool tag: %s (calls %v)", got, f.calls)
	}
	if f.tags["shared"] != testPoolAdTag || f.tags["owner"] != "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa" {
		t.Fatalf("only the shared user may get the pool tag: %v", f.tags)
	}
	// Повтор того же указания ничего не вызывает.
	f.calls = nil
	if got := applyAdPause(cfg, adPauseDirective{Active: true, Tag: testPoolAdTag}); got != adPausePool || len(f.calls) != 0 {
		t.Fatalf("idempotent set: %s %v", got, f.calls)
	}

	if got := applyAdPause(cfg, adPauseDirective{}); got != adPauseOwn {
		t.Fatalf("restore: %s", got)
	}
	if _, ok := f.tags["shared"]; ok || f.tags["owner"] == "" {
		t.Fatalf("restore must remove only the pool tag of the shared user: %v", f.tags)
	}
	if _, err := os.Stat(adPausePrevFile); !os.IsNotExist(err) {
		t.Fatal("prev file must be gone after restore")
	}
}

func TestAdPauseMtproxylRestoresPreviousPersonalTag(t *testing.T) {
	f, cfg := newFakeMtproxyl(t, "shared")
	const own = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	f.tags["shared"] = own
	f.write()
	if got := applyAdPause(cfg, adPauseDirective{Active: true, Tag: testPoolAdTag}); got != adPausePool {
		t.Fatalf("set: %s", got)
	}
	// Рестарт агента посреди окна: исходная метка — в файле.
	if got := applyAdPause(cfg, adPauseDirective{}); got != adPauseOwn || f.tags["shared"] != own {
		t.Fatalf("restore previous personal tag: %s %v", got, f.tags)
	}
}

func TestAdPauseMtproxylWithoutMEUnsupported(t *testing.T) {
	f, cfg := newFakeMtproxyl(t, "shared")
	f.me = false
	f.write()
	if got := applyAdPause(cfg, adPauseDirective{Active: true, Tag: testPoolAdTag}); got != adPauseUnsupported || len(f.calls) != 0 {
		t.Fatalf("without ME the tag is never sent: %s %v", got, f.calls)
	}
}

func TestAdPauseMtproxylOldCLIUnsupported(t *testing.T) {
	f, cfg := newFakeMtproxyl(t, "shared")
	f.fail = fmt.Errorf("mtproxyl secret adtag shared x: exit status 1: Неизвестная команда: adtag")
	if got := applyAdPause(cfg, adPauseDirective{Active: true, Tag: testPoolAdTag}); got != adPauseUnsupported {
		t.Fatalf("old MTProxyL: %s", got)
	}
	f.fail = fmt.Errorf("mtproxyl secret adtag shared x: exit status 1: boom")
	if got := applyAdPause(cfg, adPauseDirective{Active: true, Tag: testPoolAdTag}); got != adPauseFailed {
		t.Fatalf("other errors fail: %s", got)
	}
}

// Ничего не ставили — возврат ничего не вызывает.
func TestAdPauseMtproxylOwnWithoutPrevIsNoop(t *testing.T) {
	f, cfg := newFakeMtproxyl(t, "shared")
	if got := applyAdPause(cfg, adPauseDirective{}); got != adPauseOwn || len(f.calls) != 0 {
		t.Fatalf("own without prior pool tag: %s %v", got, f.calls)
	}
}
