#!/usr/bin/env bash
set -euo pipefail

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
tmpdir="$(mktemp -d)"
trap 'rm -rf -- "$tmpdir"' EXIT
export GIT_CONFIG_NOSYSTEM=1 GIT_CONFIG_GLOBAL="$tmpdir/gitconfig"
printf '[commit]\n\tgpgsign = false\n' > "$GIT_CONFIG_GLOBAL"

fail() { printf '%s\n' "FAIL: $*" >&2; exit 1; }
assert_rejected() {
  if "$@" >"$tmpdir/output" 2>&1; then
    fail "accepted invalid invocation: $*"
  fi
}
assert_matrix() {
  local expected="$1"
  local actual
  actual="$("$fixture/scripts/package-release.sh" matrix | jq -c '.include | map(.package) | sort')" || fail 'matrix failed'
  [[ "$actual" == "$expected" ]] || fail "matrix: expected $expected, got $actual"
}
set_config() {
  jq "$@" "$fixture/packages.json" > "$tmpdir/config.next"
  mv "$tmpdir/config.next" "$fixture/packages.json"
}

# The checked-in configuration is itself an acceptance case, including the disabled recipe.
actual="$("$repo_root/scripts/package-release.sh" matrix | jq -c '.include | map(.package) | sort')"
[[ "$actual" == '["aio-coding-hub-bin","tokscale-bin","zigfetch"]' ]] || fail "checked-in matrix: $actual"
assert_rejected "$repo_root/scripts/package-release.sh" matrix floral-notepaper
[[ -f "$repo_root/packages/floral-notepaper/PKGBUILD" && -f "$repo_root/packages/floral-notepaper/.SRCINFO" ]] || fail 'disabled historical metadata missing'

fixture="$tmpdir/repo"
mkdir -p "$fixture/scripts" "$fixture/packages/fixture-bin" "$fixture/packages/other-bin"
cp "$repo_root/scripts/package-release.sh" "$fixture/scripts/package-release.sh"
cat > "$fixture/packages/fixture-bin/PKGBUILD" <<'PKG'
pkgname=fixture-bin
pkgver=1.2.0
pkgrel=1
arch=('x86_64')
package() { install -d "$pkgdir/usr/share/fixture-bin"; }
PKG
cp "$fixture/packages/fixture-bin/PKGBUILD" "$fixture/packages/other-bin/PKGBUILD"
cat > "$fixture/packages/fixture-bin/.SRCINFO" <<'SRC'
pkgbase = fixture-bin
	pkgver = 1.2.0
	pkgrel = 1
	arch = x86_64
pkgname = fixture-bin
SRC
sed 's/fixture-bin/other-bin/g' "$fixture/packages/fixture-bin/.SRCINFO" > "$fixture/packages/other-bin/.SRCINFO"
cat > "$fixture/packages.json" <<'JSON'
{"packages":[{"name":"fixture-bin","enabled":false,"upstream_repo":"sample/fixture","tag_prefix":"v"}]}
JSON
assert_matrix '[]'
assert_rejected "$fixture/scripts/package-release.sh" matrix fixture-bin
set_config '.packages[0].enabled = true'
assert_matrix '["fixture-bin"]'
[[ "$("$fixture/scripts/package-release.sh" matrix fixture-bin | jq -c '.include | map(.package)')" == '["fixture-bin"]' ]] || fail 'explicit enabled selection failed'
assert_rejected "$fixture/scripts/package-release.sh" matrix missing-bin

# Renaming/adding a package changes selection without modifying the script.
set_config '.packages[0].name = "other-bin"'
assert_matrix '["other-bin"]'
set_config '.packages[0].enabled = false'
assert_matrix '[]'
assert_rejected "$fixture/scripts/package-release.sh" matrix other-bin
set_config '.packages[0].enabled = true | .packages += [.packages[0]]'
assert_rejected "$fixture/scripts/package-release.sh" matrix
set_config '.packages = [.packages[0]] | .packages[0].enabled = "true"'
assert_rejected "$fixture/scripts/package-release.sh" matrix
set_config '.packages[0].enabled = true | .packages[0].aur_extra_files = ["../escape"]'
assert_rejected "$fixture/scripts/package-release.sh" matrix
set_config '.packages[0].aur_extra_files = ["src/generated"]'
assert_rejected "$fixture/scripts/package-release.sh" matrix
set_config '.packages[0].aur_extra_files = [] | .packages[0].mystery = 1'
assert_rejected "$fixture/scripts/package-release.sh" matrix
set_config 'del(.packages[0].mystery)'
ln -s /etc/passwd "$fixture/packages/other-bin/linked"
set_config '.packages[0].aur_extra_files = ["linked"]'
assert_rejected "$fixture/scripts/package-release.sh" matrix
set_config '.packages[0].aur_extra_files = [] | .packages[0].name = "absent-bin"'
assert_rejected "$fixture/scripts/package-release.sh" matrix
set_config '.packages[0].name = "other-bin"'
mv "$fixture/packages/other-bin/.SRCINFO" "$fixture/missing-srcinfo"
assert_rejected "$fixture/scripts/package-release.sh" matrix
mv "$fixture/missing-srcinfo" "$fixture/packages/other-bin/.SRCINFO"
printf '{"packages": [' > "$fixture/packages.json"
assert_rejected "$fixture/scripts/package-release.sh" matrix
printf '%s\n' 'package configuration behavior: OK'
