#!/usr/bin/env bash
set -euo pipefail

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$repo_root"

tmpdir="$(mktemp -d)"
cleanup() {
  rm -rf "$tmpdir"
}
trap cleanup EXIT

configure_git_identity() {
  local repo="$1"

  git -C "$repo" config user.name "test"
  git -C "$repo" config user.email "test@example.com"
  git -C "$repo" config commit.gpgsign false
}

create_git_workspace() {
  local workspace="$1"

  mkdir -p "$workspace/packages/aio-coding-hub-bin"
  cp packages/aio-coding-hub-bin/PKGBUILD "$workspace/packages/aio-coding-hub-bin/PKGBUILD"
  cp packages/aio-coding-hub-bin/.SRCINFO "$workspace/packages/aio-coding-hub-bin/.SRCINFO"

  git -C "$workspace" init -q
  configure_git_identity "$workspace"
  git -C "$workspace" add .
  git -C "$workspace" commit -q -m "Initial package metadata"
}

add_tracked_build_artifacts() {
  local workspace="$1"

  mkdir -p "$workspace/packages/aio-coding-hub-bin/pkg/aio-coding-hub-bin" "$workspace/packages/aio-coding-hub-bin/src"
  printf 'package archive\n' > "$workspace/packages/aio-coding-hub-bin/aio-coding-hub-bin-0.40.8-1-x86_64.pkg.tar.zst"
  printf 'source archive\n' > "$workspace/packages/aio-coding-hub-bin/src/aio-coding-hub-0.40.8-linux-amd64.deb"
  printf 'build info\n' > "$workspace/packages/aio-coding-hub-bin/pkg/aio-coding-hub-bin/.BUILDINFO"

  git -C "$workspace" add .
  git -C "$workspace" commit -q -m "Accidentally track build artifacts"
}

create_aur_remote_with_matching_metadata() {
  local workspace="$1"
  local aur_worktree="$2"
  local aur_remote="$3"

  git init --bare -q "$aur_remote"
  git -C "$aur_worktree" init -q
  configure_git_identity "$aur_worktree"

  cp "$workspace/packages/aio-coding-hub-bin/PKGBUILD" "$aur_worktree/PKGBUILD"
  cp "$workspace/packages/aio-coding-hub-bin/.SRCINFO" "$aur_worktree/.SRCINFO"

  git -C "$aur_worktree" add .
  git -C "$aur_worktree" commit -q -m "Initial AUR metadata"
  git -C "$aur_worktree" remote add origin "$aur_remote"
  git -C "$aur_worktree" push -q origin HEAD:master
  git --git-dir="$aur_remote" symbolic-ref HEAD refs/heads/master
}

assert_contains() {
  local path="$1"
  local pattern="$2"

  grep -Fq "$pattern" "$path" || {
    echo "Expected '$pattern' in $path" >&2
    exit 1
  }
}

assert_not_contains() {
  local path="$1"
  local pattern="$2"

  if grep -Fq "$pattern" "$path"; then
    echo "Did not expect '$pattern' in $path" >&2
    exit 1
  fi
}

test_push_aur_preserves_existing_files() {
  local workspace="$tmpdir/workspace-preserve"
  local aur_worktree="$tmpdir/aur-worktree-preserve"
  local aur_remote="$tmpdir/aur-preserve.git"
  local output="$tmpdir/push-aur-preserve.out"

  mkdir -p "$workspace" "$aur_worktree"
  create_git_workspace "$workspace"
  create_aur_remote_with_matching_metadata "$workspace" "$aur_worktree" "$aur_remote"

  printf "Repository license\n" > "$aur_worktree/LICENSE"
  printf "Upstream license\n" > "$aur_worktree/tokscale-LICENSE"
  git -C "$aur_worktree" add LICENSE tokscale-LICENSE
  git -C "$aur_worktree" commit -q -m "Add AUR license files"
  git -C "$aur_worktree" push -q origin HEAD:master

  sed -i -E "s/^pkgrel=.*/pkgrel=2/" "$workspace/packages/aio-coding-hub-bin/PKGBUILD"
  printf "Updated upstream license\n" > "$workspace/packages/aio-coding-hub-bin/tokscale-LICENSE"

  if ! GIT_CONFIG_GLOBAL="/dev/null" \
    GIT_CONFIG_NOSYSTEM="1" \
    GITHUB_WORKSPACE="$workspace" \
    PACKAGE_DIR="packages/aio-coding-hub-bin" \
    AUR_EXTRA_FILES="tokscale-LICENSE" \
    AUR_REMOTE_URL="$aur_remote" \
    AUR_TARGET_BRANCH="master" \
    LATEST_VERSION="0.40.8" \
    scripts/package-release.sh push-aur >"$output" 2>&1; then
    cat "$output" >&2
    exit 1
  fi

  [[ "$(git --git-dir="$aur_remote" show master:LICENSE)" == "Repository license" ]]
  [[ "$(git --git-dir="$aur_remote" show master:tokscale-LICENSE)" == "Updated upstream license" ]]
  [[ "$(git --git-dir="$aur_remote" show master:PKGBUILD)" == *"pkgrel=2"* ]]
}

test_push_aur_ignores_tracked_build_artifacts() {
  local workspace="$tmpdir/workspace-artifacts"
  local aur_worktree="$tmpdir/aur-worktree-artifacts"
  local aur_remote="$tmpdir/aur-artifacts.git"
  local output="$tmpdir/push-aur-artifacts.out"
  local pushed_files="$tmpdir/push-aur-artifacts-files.out"

  mkdir -p "$workspace" "$aur_worktree"
  create_git_workspace "$workspace"
  add_tracked_build_artifacts "$workspace"
  create_aur_remote_with_matching_metadata "$workspace" "$aur_worktree" "$aur_remote"

  sed -i -E "s/^pkgrel=.*/pkgrel=2/" "$workspace/packages/aio-coding-hub-bin/PKGBUILD"
  git -C "$workspace" add packages/aio-coding-hub-bin/PKGBUILD
  git -C "$workspace" commit -q -m "Update package metadata"

  if ! GIT_CONFIG_GLOBAL="/dev/null" \
    GIT_CONFIG_NOSYSTEM="1" \
    GITHUB_WORKSPACE="$workspace" \
    PACKAGE_DIR="packages/aio-coding-hub-bin" \
    AUR_REMOTE_URL="$aur_remote" \
    AUR_TARGET_BRANCH="master" \
    LATEST_VERSION="0.40.8" \
    scripts/package-release.sh push-aur >"$output" 2>&1; then
    cat "$output" >&2
    exit 1
  fi

  git --git-dir="$aur_remote" ls-tree -r --name-only master > "$pushed_files"
  assert_contains "$pushed_files" "PKGBUILD"
  assert_contains "$pushed_files" ".SRCINFO"
  assert_not_contains "$pushed_files" ".pkg.tar.zst"
  assert_not_contains "$pushed_files" "src/"
  assert_not_contains "$pushed_files" "pkg/"
}

test_push_github_ignores_tracked_build_artifacts() {
  local workspace="$tmpdir/workspace-github"
  local output="$tmpdir/push-github.out"
  local pushed_files="$tmpdir/push-github-files.out"

  mkdir -p "$workspace"
  create_git_workspace "$workspace"
  add_tracked_build_artifacts "$workspace"

  sed -i -E "s/^pkgrel=.*/pkgrel=2/" "$workspace/packages/aio-coding-hub-bin/PKGBUILD"

  git -C "$workspace" init --bare -q "$tmpdir/github.git"
  git -C "$workspace" remote add origin "$tmpdir/github.git"

  if ! GIT_CONFIG_GLOBAL="/dev/null" \
    GIT_CONFIG_NOSYSTEM="1" \
    GITHUB_WORKSPACE="$workspace" \
    PACKAGE_DIR="packages/aio-coding-hub-bin" \
    GITHUB_TARGET_BRANCH="main" \
    PACKAGE_NAME="aio-coding-hub-bin" \
    LATEST_VERSION="0.40.8" \
    scripts/package-release.sh push-github >"$output" 2>&1; then
    cat "$output" >&2
    exit 1
  fi

  git --git-dir="$tmpdir/github.git" log -1 --name-only --pretty=format: refs/heads/main > "$pushed_files"
  assert_contains "$pushed_files" "packages/aio-coding-hub-bin/PKGBUILD"
  assert_not_contains "$pushed_files" ".pkg.tar.zst"
  assert_not_contains "$pushed_files" "src/"
  assert_not_contains "$pushed_files" "pkg/"
}

test_push_aur_preserves_existing_files
test_push_aur_ignores_tracked_build_artifacts
test_push_github_ignores_tracked_build_artifacts
