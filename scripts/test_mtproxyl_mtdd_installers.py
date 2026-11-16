#!/usr/bin/env python3
"""Installer regressions without systemd, proxy changes or a live registry."""
import json
import os
from pathlib import Path
import subprocess
import tempfile
import unittest


ROOT = Path(__file__).resolve().parents[1]


def function(script, name):
    source = script.read_text()
    start = source.index(name + "() {")
    return source[start:source.index("\n}", start) + 2]


class ManagedMtddInstallerTest(unittest.TestCase):
    def test_both_installers(self):
        for name in ("install_node.sh", "install_node_web.sh"):
            with self.subTest(installer=name), tempfile.TemporaryDirectory() as tmp:
                root = Path(tmp)
                (root / "mtproxy").mkdir()
                (root / "mtdd").mkdir()
                # A previous telemt config must not override the selected engine.
                (root / "mtproxy/config.toml").write_text("[server]\nport=443\n")
                (root / "settings.conf").write_text("MTPROXYL_MODE='manager'\nENGINE_KIND='mtdd'\n")
                (root / "mtdd/config.json").write_text(json.dumps({
                    "control_port": 9091, "metrics_host": "::1", "metrics_port": 9092,
                    "metrics_token_env": "METRICS_TOKEN",
                }))
                token = "a" * 64
                (root / "mtdd/credentials.env").write_text("METRICS_TOKEN='" + token + "'\n")
                script = ROOT / "scripts" / name
                functions = "\n".join(function(script, fn) for fn in (
                    "detect_mtproxyl_config", "configure_mtdd_metrics", "apply_mtproxyl_from_registry", "update_mtdd_config_path",
                ))
                body = r'''
set -euo pipefail
MTPROXYL_DIR="$1"; MTPROXYL_SETTINGS="$1/settings.conf"
die() { echo "$*" >&2; exit 1; }
say() { :; }; mtproxyl_cli() { return 0; }
mtproxyl() { echo 'unexpected CLI bootstrap' >&2; exit 19; }
curl() { echo 'unexpected registry bootstrap' >&2; exit 20; }
TELEMT_MTPROXYL=$(detect_mtproxyl_config)
test "$TELEMT_MTPROXYL" = "$MTPROXYL_DIR/mtdd/config.json"
TELEMT_CONFIG="$TELEMT_MTPROXYL"; MTPROXYL_MODE=1
apply_mtproxyl_from_registry
printf '[registry]\nurl="http://example.org"\n[telemt]\nconfig_path="/etc/telemt/telemt.toml"\n[sync]\napply_to_telemt=true\n' > "$1/node.toml"
update_mtdd_config_path "$1/node.toml"
grep -q 'config_path = "/.*/mtdd/config.json"' "$1/node.toml"
grep -q 'apply_to_telemt=true' "$1/node.toml"
configure_mtdd_metrics
test "$METRICS_URL" = 'http://[::1]:9092/metrics'
test "${METRICS_AUTH[0]}" = "Authorization: Bearer $EXPECTED_TOKEN"
# As MTProxyL writes it: metrics listener without a token (metrics_allow),
# never the deprecated /metrics on the API port.
printf '{"control_port":9091,"api_token_env":"MTDD_API_TOKEN","metrics_host":"0.0.0.0","metrics_port":9090}' > "$1/mtdd/config.json"
configure_mtdd_metrics
test "$METRICS_URL" = 'http://127.0.0.1:9090/metrics'
test "${#METRICS_AUTH[@]}" = 0
# Without metrics_port: legacy /metrics on the API port with the admin token.
printf '{"control_port":9091,"metrics_port":0}' > "$1/mtdd/config.json"
printf 'MTDD_API_TOKEN=%s\n' "$EXPECTED_TOKEN" > "$1/mtdd/credentials.env"
configure_mtdd_metrics
test "$METRICS_URL" = 'http://127.0.0.1:9091/metrics'
test "${METRICS_AUTH[0]}" = "Authorization: Bearer $EXPECTED_TOKEN"
printf "ENGINE_KIND='telemt'\n" > "$MTPROXYL_SETTINGS"
test "$(detect_mtproxyl_config)" = "$MTPROXYL_DIR/mtproxy/config.toml"
'''
                env = dict(os.environ, EXPECTED_TOKEN=token)
                subprocess.run(["bash", "-c", functions + "\n" + body, "--", str(root)],
                               check=True, env=env, capture_output=True, text=True)


if __name__ == "__main__":
    unittest.main()
