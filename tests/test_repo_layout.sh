#!/usr/bin/env bash
set -euo pipefail

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$repo_root"

assert_file() {
  local path="$1"
  [[ -f "$path" ]] || {
    echo "Missing file: $path" >&2
    exit 1
  }
}

assert_not_file() {
  local path="$1"
  [[ ! -f "$path" ]] || {
    echo "Unexpected root-level file still exists: $path" >&2
    exit 1
  }
}

assert_contains() {
  local path="$1"
  local pattern="$2"
  grep -Fq "$pattern" "$path" || {
    echo "Expected '$pattern' in $path" >&2
    exit 1
  }
}

assert_file "packages/aio-coding-hub-bin/PKGBUILD"
assert_file "packages/aio-coding-hub-bin/.SRCINFO"
assert_file "packages/floral-notepaper/PKGBUILD"
assert_file "packages/floral-notepaper/.SRCINFO"
assert_file "packages/zigfetch/PKGBUILD"
assert_file "packages/zigfetch/.SRCINFO"
assert_file ".github/workflows/check-aio-coding-hub-bin-upstream-release.yml"
assert_file ".github/workflows/check-floral-notepaper-upstream-release.yml"
assert_file ".github/workflows/check-zigfetch-upstream-release.yml"
assert_file "scripts/package-release.sh"

assert_not_file "PKGBUILD"
assert_not_file ".SRCINFO"

assert_contains ".github/workflows/check-aio-coding-hub-bin-upstream-release.yml" "PACKAGE_DIR: packages/aio-coding-hub-bin"
assert_contains ".github/workflows/check-floral-notepaper-upstream-release.yml" "PACKAGE_DIR: packages/floral-notepaper"
assert_contains ".github/workflows/check-zigfetch-upstream-release.yml" "PACKAGE_DIR: packages/zigfetch"
assert_contains ".github/workflows/check-aio-coding-hub-bin-upstream-release.yml" "AUR_REMOTE_URL: ssh://aur@aur.archlinux.org/aio-coding-hub-bin.git"
assert_contains ".github/workflows/check-floral-notepaper-upstream-release.yml" "AUR_REMOTE_URL: ssh://aur@aur.archlinux.org/floral-notepaper.git"
assert_contains ".github/workflows/check-zigfetch-upstream-release.yml" "AUR_REMOTE_URL: ssh://aur@aur.archlinux.org/zigfetch.git"
