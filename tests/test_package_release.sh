#!/usr/bin/env bash
set -euo pipefail

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
tmpdir="$(mktemp -d)"
trap 'rm -rf -- "$tmpdir"' EXIT
[[ "$(id -u)" -ne 0 ]] || { echo 'Run release behavior tests as a non-root user' >&2; exit 1; }
for tool in git jq makepkg vercmp ssh-keygen; do
  command -v "$tool" >/dev/null || { echo "Missing test prerequisite: $tool" >&2; exit 1; }
done
real_git="$(command -v git)"
mkdir -p "$tmpdir/bin"
cat > "$tmpdir/bin/curl" <<'SH'
#!/usr/bin/env bash
[[ "${CURL_FAIL:-}" != true ]] || exit 22
printf '{"tag_name":"%s"}\n' "${CURL_TAG:-v1.2.0}"
SH
chmod +x "$tmpdir/bin/curl"
export PATH="$tmpdir/bin:$PATH" GIT_CONFIG_NOSYSTEM=1 GIT_CONFIG_GLOBAL="$tmpdir/global.gitconfig"
printf '[commit]\n\tgpgsign = false\n' > "$GIT_CONFIG_GLOBAL"
ssh-keygen -q -t ed25519 -N '' -f "$tmpdir/test-key" >/dev/null
export AUR_SSH_PRIVATE_KEY
AUR_SSH_PRIVATE_KEY="$(<"$tmpdir/test-key")"
export GITHUB_TOKEN=test-only-local-transport GITHUB_TARGET_BRANCH=main

fail() { printf 'FAIL: %s\n' "$*" >&2; exit 1; }
assert_fails() {
  if "$@" >"$tmpdir/last-output" 2>&1; then
    fail "unexpected success: $*"
  fi
}
assert_output() {
  [[ "$(<"$tmpdir/last-output")" == *"$1"* ]] || fail "expected '$1' in: $(<"$tmpdir/last-output")"
}
identity() {
  git -C "$1" config user.name 'Maintenance Test'
  git -C "$1" config user.email 'maintenance@example.invalid'
  git -C "$1" config commit.gpgsign false
}
make_recipe() {
  local name="$1" version="${2:-1.2.0}" rel="${3:-1}" checksum
  mkdir -p "$repo/packages/$name"
  printf 'Offline source for %s\n' "$name" > "$repo/packages/$name/fixture-source"
  checksum="$(sha256sum "$repo/packages/$name/fixture-source")"
  checksum="${checksum%% *}"
  cat > "$repo/packages/$name/PKGBUILD" <<PKG
pkgname=$name
pkgver=$version
pkgrel=$rel
pkgdesc='Local package maintenance fixture'
arch=('x86_64')
license=('MIT')
source=('fixture-source')
sha256sums=('$checksum')
package() { install -d "\$pkgdir/usr/share/$name"; }
PKG
  (cd "$repo/packages/$name" && makepkg --printsrcinfo > .SRCINFO)
}
# Every fixture runs with its own Git identity, remotes, and URL mappings. Production
# scripts see canonical AUR URLs, never a test-only remote override.
init_fixture() {
  local count="${1:-1}" name
  fixture="$(mktemp -d "$tmpdir/fixture.XXXXXX")"
  repo="$fixture/repo"
  mkdir -p "$repo/scripts" "$repo/.github/workflows"
  cp "$repo_root/scripts/package-release.sh" "$repo/scripts/package-release.sh"
  cp "$repo_root/.github/aur_known_hosts" "$repo/.github/aur_known_hosts"
  for name in maintain-packages.yml release-package.yml; do
    cp "$repo_root/.github/workflows/$name" "$repo/.github/workflows/$name"
  done
  make_recipe fixture-bin
  if (( count == 2 )); then make_recipe other-bin; fi
  jq -n --argjson count "$count" '{packages: ([{name:"fixture-bin",enabled:true,upstream_repo:"fixture/fixture",tag_prefix:"v",aur_extra_files:["fixture-source"]}] + (if $count == 2 then [{name:"other-bin",enabled:true,upstream_repo:"fixture/other",tag_prefix:"v",aur_extra_files:["fixture-source"]}] else [] end))}' > "$repo/packages.json"
  git -C "$repo" init -q -b main
  identity "$repo"
  git -C "$repo" add .
  git -C "$repo" commit -q -m 'Trusted source commit'
  github="$fixture/github.git"
  git init --bare -q "$github"
  git --git-dir="$github" symbolic-ref HEAD refs/heads/main
  git -C "$repo" remote add origin 'https://github.com/fixture/maintained.git'
  export GIT_CONFIG_GLOBAL="$fixture/gitconfig"
  git config --file "$GIT_CONFIG_GLOBAL" 'url.'"$github"'.insteadOf' 'https://github.com/fixture/maintained.git'
  git -C "$repo" push -q origin HEAD:main
  create_aur fixture-bin
  if (( count == 2 )); then create_aur other-bin; fi
  export GIT_CONFIG_GLOBAL="$fixture/gitconfig" CURL_TAG=v1.2.0
  unset CURL_FAIL
  source_commit="$(git -C "$repo" rev-parse HEAD)"
}
create_aur() {
  local name="$1" remote="$fixture/$1.git" seed="$fixture/seed-$1"
  git init --bare -q "$remote"
  git --git-dir="$remote" symbolic-ref HEAD refs/heads/master
  git init -q -b master "$seed"
  identity "$seed"
  cp "$repo/packages/$name/PKGBUILD" "$repo/packages/$name/.SRCINFO" "$repo/packages/$name/fixture-source" "$seed/"
  git -C "$seed" add .
  git -C "$seed" commit -q -m 'AUR baseline'
  git -C "$seed" remote add origin "$remote"
  git -C "$seed" push -q origin HEAD:master
  git config --file "$fixture/gitconfig" --add 'url.'"$remote"'.insteadOf' "https://aur.archlinux.org/$name.git"
  git config --file "$fixture/gitconfig" --add 'url.'"$remote"'.insteadOf' "ssh://aur@aur.archlinux.org/$name.git"
}
prepare() {
  local name="${1:-fixture-bin}" dest
  dest="${2:-$fixture/candidate-$name}"
  SOURCE_COMMIT="$source_commit" "$repo/scripts/package-release.sh" prepare "$name" "$dest"
}
publish() {
  local name="${1:-fixture-bin}" dest
  dest="${2:-$fixture/candidate-$name}"
  SOURCE_COMMIT="$source_commit" "$repo/scripts/package-release.sh" publish "$name" "$dest"
}
expect_status() {
  local output="$1" key="$2" expected="$3"
  [[ "$output" == *"$key=$expected"* ]] || fail "expected $key=$expected; got: $output"
}
set_upstream() { export CURL_TAG="v$1"; }
add_unrelated_artifacts() {
  mkdir -p "$repo/packages/fixture-bin/src" "$repo/packages/fixture-bin/pkg"
  printf 'download\n' > "$repo/packages/fixture-bin/src/archive"
  printf 'build\n' > "$repo/packages/fixture-bin/pkg/archive"
  printf 'archive\n' > "$repo/packages/fixture-bin/fixture-bin-1.pkg.tar.zst"
  git -C "$repo" add .
  git -C "$repo" commit -q -m 'Tracked unrelated build output'
  git -C "$repo" push -q origin HEAD:main
  source_commit="$(git -C "$repo" rev-parse HEAD)"
}
assert_aur_file() {
  local name="$1" file="$2" expected="$3"
  [[ "$(git --git-dir="$fixture/$name.git" show "master:$file")" == "$expected" ]] || fail "incorrect AUR $name:$file"
}

# A genuinely empty AUR repository is the only permitted initial-publish baseline.
init_fixture
git --git-dir="$fixture/fixture-bin.git" update-ref -d refs/heads/master
out="$(prepare)"; expect_status "$out" needs_build true; expect_status "$out" needs_publish true
publish > /dev/null
assert_aur_file fixture-bin fixture-source 'Offline source for fixture-bin'
[[ "$(git --git-dir="$github" rev-parse main)" == "$source_commit" ]] || fail 'initial AUR publish changed GitHub'

# Numeric ordering must come from vercmp; unchanged or older tags preserve pkgrel.
init_fixture
out="$(prepare)"; expect_status "$out" needs_build false; expect_status "$out" needs_publish false
[[ "$(jq -r '.version' "$fixture/candidate-fixture-bin/manifest.json")" == '1.2.0-1' ]] || fail 'incorrect candidate version'
init_fixture
set_upstream 1.3.0
out="$(prepare)"; expect_status "$out" needs_build true; expect_status "$out" needs_publish true
[[ "$(jq -r '.version' "$fixture/candidate-fixture-bin/manifest.json")" == '1.3.0-1' ]] || fail 'upgrade did not reset pkgrel'
init_fixture
make_recipe fixture-bin 1.3.0 2
(cd "$repo" && git add packages/fixture-bin && git commit -q -m 'Bump recipe' && git push -q origin HEAD:main)
source_commit="$(git -C "$repo" rev-parse HEAD)"
set_upstream 1.2.0
out="$(prepare)"; expect_status "$out" needs_build true; expect_status "$out" needs_publish true
[[ "$(jq -r '.version' "$fixture/candidate-fixture-bin/manifest.json")" == '1.3.0-2' ]] || fail 'older tag downgraded pkgrel'
init_fixture
make_recipe fixture-bin 1.2.0 2
(cd "$repo" && git add packages/fixture-bin && git commit -q -m 'Bump pkgrel' && git push -q origin HEAD:main)
source_commit="$(git -C "$repo" rev-parse HEAD)"
out="$(prepare)"; expect_status "$out" needs_build true; expect_status "$out" needs_publish true
[[ "$(jq -r '.version' "$fixture/candidate-fixture-bin/manifest.json")" == '1.2.0-2' ]] || fail 'pkgrel change lost'
init_fixture
sed -i '/^pkgver=/a epoch=1' "$repo/packages/fixture-bin/PKGBUILD"
(cd "$repo/packages/fixture-bin" && makepkg --printsrcinfo > .SRCINFO)
(cd "$repo" && git add packages/fixture-bin && git commit -q -m 'Epoch bump' && git push -q origin HEAD:main)
source_commit="$(git -C "$repo" rev-parse HEAD)"
out="$(prepare)"; expect_status "$out" needs_build true
[[ "$(jq -r '.version' "$fixture/candidate-fixture-bin/manifest.json")" == '1:1.2.0-1' ]] || fail 'epoch lost from candidate version'
init_fixture
sed -i 's/^pkgrel=1$/pkgrel=2/' "$fixture/seed-fixture-bin/PKGBUILD"
(cd "$fixture/seed-fixture-bin" && makepkg --printsrcinfo > .SRCINFO)
(cd "$fixture/seed-fixture-bin" && git add . && git commit -q -m 'AUR version ahead' && git push -q origin HEAD:master)
assert_fails prepare
init_fixture
sed -i 's/Local package maintenance fixture/Different same-version recipe/' "$fixture/seed-fixture-bin/PKGBUILD"
(cd "$fixture/seed-fixture-bin" && git add PKGBUILD && git commit -q -m 'AUR same-version recipe changed' && git push -q origin HEAD:master)
assert_fails prepare
assert_output 'pkgrel'
init_fixture
printf '\toptdepends = optional-package\n' >> "$fixture/seed-fixture-bin/.SRCINFO"
(cd "$fixture/seed-fixture-bin" && git add .SRCINFO && git commit -q -m 'Stale AUR metadata' && git push -q origin HEAD:master)
out="$(prepare)"; expect_status "$out" needs_build true; expect_status "$out" needs_publish true
publish > /dev/null
[[ "$(git --git-dir="$fixture/fixture-bin.git" show master:.SRCINFO)" == "$(<"$repo/packages/fixture-bin/.SRCINFO")" ]] || fail 'AUR metadata was not repaired'
[[ "$(git --git-dir="$github" rev-parse main)" == "$source_commit" ]] || fail 'AUR-only repair wrote to GitHub'
init_fixture
rm "$fixture/seed-fixture-bin/.SRCINFO"
(cd "$fixture/seed-fixture-bin" && git add -u && git commit -q -m 'AUR metadata missing' && git push -q origin HEAD:master)
assert_fails prepare
init_fixture
out="$(FORCE_BUILD=true prepare)"; expect_status "$out" needs_build true; expect_status "$out" needs_publish false
init_fixture
set_upstream invalid
assert_fails prepare
export CURL_FAIL=true
assert_fails prepare
unset CURL_FAIL

# Candidate bytes, completeness, and confinement are validated before publication.
init_fixture
set_upstream 1.3.0
prepare > /dev/null
cp -a "$fixture/candidate-fixture-bin" "$fixture/tampered"
printf 'changed\n' >> "$fixture/tampered/package/PKGBUILD"
assert_fails publish fixture-bin "$fixture/tampered"
cp -a "$fixture/candidate-fixture-bin" "$fixture/missing"
rm "$fixture/missing/package/.SRCINFO"
assert_fails publish fixture-bin "$fixture/missing"
cp -a "$fixture/candidate-fixture-bin" "$fixture/linked"
rm "$fixture/linked/package/.SRCINFO"
ln -s "$repo/packages/fixture-bin/.SRCINFO" "$fixture/linked/package/.SRCINFO"
assert_fails publish fixture-bin "$fixture/linked"
cp -a "$fixture/candidate-fixture-bin" "$fixture/extra"
printf 'hidden\n' > "$fixture/extra/package/surprise"
assert_fails publish fixture-bin "$fixture/extra"
ln -s "$fixture/candidate-fixture-bin" "$fixture/candidate-link"
assert_fails publish fixture-bin "$fixture/candidate-link"
assert_fails env SOURCE_COMMIT=0000000000000000000000000000000000000000 "$repo/scripts/package-release.sh" publish fixture-bin "$fixture/candidate-fixture-bin"
[[ "$(git --git-dir="$github" rev-parse main)" == "$source_commit" ]] || fail 'invalid candidate changed GitHub'
[[ "$(git --git-dir="$fixture/fixture-bin.git" rev-parse master)" == "$(git -C "$fixture/seed-fixture-bin" rev-parse HEAD)" ]] || fail 'invalid candidate changed AUR'

# Broken dependency metadata must never be interpreted as an empty dependency list.
cat > "$tmpdir/bin/pacman" <<'SH'
#!/usr/bin/env bash
printf '%s\n' "$@" >> "$PACMAN_LOG"
exit "${PACMAN_EXIT:-0}"
SH
chmod +x "$tmpdir/bin/pacman"
export PACMAN_LOG="$tmpdir/pacman-log"
: > "$PACMAN_LOG"
rm "$fixture/candidate-fixture-bin/package/.SRCINFO"
assert_fails env SOURCE_COMMIT="$source_commit" "$repo/scripts/package-release.sh" install-build-deps fixture-bin "$fixture/candidate-fixture-bin"
[[ ! -s "$PACMAN_LOG" ]] || fail 'pacman ran with missing metadata'
assert_fails env SOURCE_COMMIT="$source_commit" "$repo/scripts/package-release.sh" install-build-deps fixture-bin "$fixture/linked"
[[ ! -s "$PACMAN_LOG" ]] || fail 'pacman ran with linked metadata'
init_fixture
prepare > /dev/null
SOURCE_COMMIT="$source_commit" "$repo/scripts/package-release.sh" install-build-deps fixture-bin "$fixture/candidate-fixture-bin" > /dev/null
[[ ! -s "$PACMAN_LOG" ]] || fail 'pacman ran with empty dependency list'
init_fixture
# A real .SRCINFO dependency is parsed, stripped of its constraint for pacman, and its error propagates.
printf "\ndepends=('missing-fixture-package>=2')\ndepends_x86_64=('arch-fixture-package<4')\n" >> "$repo/packages/fixture-bin/PKGBUILD"
sed -i 's/^pkgrel=1$/pkgrel=2/' "$repo/packages/fixture-bin/PKGBUILD"
(cd "$repo/packages/fixture-bin" && makepkg --printsrcinfo > .SRCINFO)
(cd "$repo" && git add packages/fixture-bin && git commit -q -m 'Declare dependency' && git push -q origin HEAD:main)
source_commit="$(git -C "$repo" rev-parse HEAD)"
prepare > /dev/null
export PACMAN_EXIT=43
assert_fails env SOURCE_COMMIT="$source_commit" "$repo/scripts/package-release.sh" install-build-deps fixture-bin "$fixture/candidate-fixture-bin"
[[ -s "$PACMAN_LOG" ]] || fail 'dependency install was not attempted'
grep -Fxq 'missing-fixture-package' "$PACMAN_LOG" || fail 'versioned dependency was not passed to pacman'
grep -Fxq 'arch-fixture-package' "$PACMAN_LOG" || fail 'architecture-specific dependency was not passed to pacman'
if grep -Fq '>=' "$PACMAN_LOG"; then fail 'version comparison leaked into pacman target'; fi
unset PACMAN_EXIT

# The install fixture must not mask makepkg's real pacman -T dependency check.
mv "$tmpdir/bin/pacman" "$tmpdir/pacman-install-fixture"
# Build must let makepkg reject an unsatisfied declared dependency.
init_fixture
printf "\ndepends=('nonexistent-maintenance-fixture>=999')\n" >> "$repo/packages/fixture-bin/PKGBUILD"
sed -i 's/^pkgrel=1$/pkgrel=2/' "$repo/packages/fixture-bin/PKGBUILD"
(cd "$repo/packages/fixture-bin" && makepkg --printsrcinfo > .SRCINFO)
(cd "$repo" && git add packages/fixture-bin && git commit -q -m 'Require unavailable dependency' && git push -q origin HEAD:main)
source_commit="$(git -C "$repo" rev-parse HEAD)"
prepare > /dev/null
assert_fails env SOURCE_COMMIT="$source_commit" "$repo/scripts/package-release.sh" build fixture-bin "$fixture/candidate-fixture-bin" "$fixture/failed-build"
[[ -z "$(find "$fixture/failed-build" -maxdepth 1 -name '*.pkg.tar.*' -print -quit)" ]] || fail 'unresolved dependency produced a package'
# Both remote writes operate on real bare repositories and Git commits.
init_fixture
add_unrelated_artifacts
printf 'AUR-owned license\n' > "$fixture/seed-fixture-bin/LICENSE"
(cd "$fixture/seed-fixture-bin" && git add LICENSE && git commit -q -m 'Unmanaged AUR file' && git push -q origin HEAD:master)
set_upstream 1.3.0
prepare > /dev/null
publish > /dev/null
assert_aur_file fixture-bin LICENSE 'AUR-owned license'
[[ "$(git --git-dir="$fixture/fixture-bin.git" show master:PKGBUILD)" == *'pkgver=1.3.0'* ]] || fail 'AUR version not published'
[[ "$(git --git-dir="$github" show main:packages/fixture-bin/PKGBUILD)" == *'pkgver=1.3.0'* ]] || fail 'GitHub version not published'
for remote in "$github" "$fixture/fixture-bin.git"; do
  [[ "$(git --git-dir="$remote" log -1 --name-only --pretty=format: | sed '/^$/d' | LC_ALL=C sort)" == $(if [[ "$remote" == "$github" ]]; then printf 'packages/fixture-bin/.SRCINFO\npackages/fixture-bin/PKGBUILD' | LC_ALL=C sort; else printf '.SRCINFO\nPKGBUILD' | LC_ALL=C sort; fi) ]] || fail "unexpected paths in published commit: $remote"
done
if git --git-dir="$fixture/fixture-bin.git" cat-file -e master:src/archive 2>/dev/null; then fail 'tracked build output leaked into AUR'; fi

# A second package prepared from the same source commit can publish after the first.
init_fixture 2
set_upstream 1.3.0
prepare fixture-bin > /dev/null
prepare other-bin > /dev/null
publish fixture-bin > /dev/null
publish other-bin > /dev/null
for name in fixture-bin other-bin; do
  [[ "$(git --git-dir="$github" show "main:packages/$name/PKGBUILD")" == *'pkgver=1.3.0'* ]] || fail "GitHub lost $name"
  [[ "$(git --git-dir="$fixture/$name.git" show master:PKGBUILD)" == *'pkgver=1.3.0'* ]] || fail "AUR lost $name"
done

# Interleave another real commit immediately before push, so the first attempt
# receives a genuine non-fast-forward response from the bare repository.
cat > "$tmpdir/bin/git" <<'SH'
#!/usr/bin/env bash
cwd="$PWD"
previous=''
is_push=false
for arg in "$@"; do
  if [[ "$previous" == '-C' ]]; then cwd="$arg"; fi
  if [[ "$arg" == 'push' ]]; then is_push=true; fi
  previous="$arg"
done
if [[ "$is_push" == true && -n "${INJECT_REMOTE:-}" && ! -e "$INJECT_MARKER" ]]; then
  remote="$("$REAL_GIT" -C "$cwd" remote get-url origin 2>/dev/null || true)"
  if [[ "$remote" == "$INJECT_REMOTE" ]]; then
    : > "$INJECT_MARKER"
    "$REAL_GIT" -C "$INJECT_CLONE" push -q origin "HEAD:$INJECT_BRANCH" || exit
  fi
fi
exec "$REAL_GIT" "$@"
SH
chmod +x "$tmpdir/bin/git"
export REAL_GIT="$real_git"
init_fixture 2
set_upstream 1.3.0
prepare fixture-bin > /dev/null
INJECT_CLONE="$fixture/other-update"
git clone -q "$github" "$INJECT_CLONE"
identity "$INJECT_CLONE"
sed -i 's/^pkgrel=1$/pkgrel=2/' "$INJECT_CLONE/packages/other-bin/PKGBUILD"
(cd "$INJECT_CLONE/packages/other-bin" && makepkg --printsrcinfo > .SRCINFO)
(cd "$INJECT_CLONE" && git add packages/other-bin && git commit -q -m 'Interleaved other package')
INJECT_REMOTE="$github" INJECT_MARKER="$fixture/injected-github" INJECT_BRANCH=main
export INJECT_REMOTE INJECT_MARKER INJECT_CLONE INJECT_BRANCH
publish fixture-bin > /dev/null
[[ -e "$INJECT_MARKER" ]] || fail 'GitHub push was not interleaved'
[[ "$(git --git-dir="$github" show main:packages/other-bin/PKGBUILD)" == *'pkgrel=2'* ]] || fail 'GitHub retry lost interleaved package'
[[ "$(git --git-dir="$github" show main:packages/fixture-bin/PKGBUILD)" == *'pkgver=1.3.0'* ]] || fail 'GitHub retry did not publish candidate'
unset INJECT_REMOTE INJECT_MARKER INJECT_CLONE INJECT_BRANCH
init_fixture
set_upstream 1.3.0
prepare > /dev/null
INJECT_CLONE="$fixture/aur-update"
git clone -q "$fixture/fixture-bin.git" "$INJECT_CLONE"
identity "$INJECT_CLONE"
printf 'Concurrent AUR license\n' > "$INJECT_CLONE/LICENSE"
(cd "$INJECT_CLONE" && git add LICENSE && git commit -q -m 'Interleaved unrelated AUR file')
INJECT_REMOTE="$fixture/fixture-bin.git" INJECT_MARKER="$fixture/injected-aur" INJECT_BRANCH=master
export INJECT_REMOTE INJECT_MARKER INJECT_CLONE INJECT_BRANCH
publish > /dev/null
[[ -e "$INJECT_MARKER" ]] || fail 'AUR push was not interleaved'
assert_aur_file fixture-bin LICENSE 'Concurrent AUR license'
[[ "$(git --git-dir="$fixture/fixture-bin.git" show master:PKGBUILD)" == *'pkgver=1.3.0'* ]] || fail 'AUR retry did not publish candidate'
unset INJECT_REMOTE INJECT_MARKER INJECT_CLONE INJECT_BRANCH

# AUR success followed by GitHub refusal is red; rerunning repairs GitHub without AUR churn.
init_fixture
set_upstream 1.3.0
prepare > /dev/null
cat > "$github/hooks/pre-receive" <<'SH'
#!/usr/bin/env bash
exit 1
SH
chmod +x "$github/hooks/pre-receive"
assert_fails publish
assert_output 'AUR synchronized; GitHub synchronization failed'
aur_head="$(git --git-dir="$fixture/fixture-bin.git" rev-parse master)"
rm "$github/hooks/pre-receive"
rm -rf "$fixture/candidate-fixture-bin"
out="$(prepare)"; expect_status "$out" needs_build true; expect_status "$out" needs_publish true
publish > /dev/null
[[ "$(git --git-dir="$fixture/fixture-bin.git" rev-parse master)" == "$aur_head" ]] || fail 'recovery repushed AUR'
[[ "$(git --git-dir="$github" show main:packages/fixture-bin/PKGBUILD)" == *'pkgver=1.3.0'* ]] || fail 'recovery did not repair GitHub'

# A conflicting recipe or disabled configuration on main must stop the old candidate.
init_fixture
set_upstream 1.3.0
prepare > /dev/null
old_aur_head="$(git --git-dir="$fixture/fixture-bin.git" rev-parse master)"
conflict="$fixture/conflict"
git clone -q "$github" "$conflict"
identity "$conflict"
sed -i 's/^pkgrel=1$/pkgrel=9/' "$conflict/packages/fixture-bin/PKGBUILD"
(cd "$conflict/packages/fixture-bin" && makepkg --printsrcinfo > .SRCINFO)
(cd "$conflict" && git add packages/fixture-bin && git commit -q -m 'Concurrent same-package change' && git push -q origin HEAD:main)
assert_fails publish
[[ "$(git --git-dir="$fixture/fixture-bin.git" rev-parse master)" == "$old_aur_head" ]] || fail 'AUR changed after GitHub conflict'
init_fixture
set_upstream 1.3.0
prepare > /dev/null
old_aur_head="$(git --git-dir="$fixture/fixture-bin.git" rev-parse master)"
conflict="$fixture/conflict"
git clone -q "$github" "$conflict"
identity "$conflict"
jq '.packages[0].enabled = false' "$conflict/packages.json" > "$conflict/config.new"
mv "$conflict/config.new" "$conflict/packages.json"
(cd "$conflict" && git add packages.json && git commit -q -m 'Disable package' && git push -q origin HEAD:main)
assert_fails publish
[[ "$(git --git-dir="$fixture/fixture-bin.git" rev-parse master)" == "$old_aur_head" ]] || fail 'disabled candidate changed AUR'

# Configured source extras are verified and copied into AUR, not GitHub updates.
init_fixture
printf 'Verified local license\n' > "$repo/packages/fixture-bin/fixture-LICENSE"
checksum="$(sha256sum "$repo/packages/fixture-bin/fixture-LICENSE")"
checksum="${checksum%% *}"
sed -i 's/^pkgrel=1$/pkgrel=2/' "$repo/packages/fixture-bin/PKGBUILD"
printf "\nsource=('fixture-LICENSE')\nsha256sums=('%s')\n" "$checksum" >> "$repo/packages/fixture-bin/PKGBUILD"
(cd "$repo/packages/fixture-bin" && makepkg --printsrcinfo > .SRCINFO)
jq '.packages[0].aur_extra_files = ["fixture-LICENSE"]' "$repo/packages.json" > "$fixture/config.new"
mv "$fixture/config.new" "$repo/packages.json"
(cd "$repo" && git add packages.json packages/fixture-bin && git commit -q -m 'Track verified source extra' && git push -q origin HEAD:main)
source_commit="$(git -C "$repo" rev-parse HEAD)"
prepare > /dev/null
[[ "$(<"$fixture/candidate-fixture-bin/package/fixture-LICENSE")" == 'Verified local license' ]] || fail 'candidate missing source extra'
publish > /dev/null
assert_aur_file fixture-bin fixture-LICENSE 'Verified local license'
[[ "$(git --git-dir="$fixture/fixture-bin.git" log -1 --name-only --pretty=format: | sed '/^$/d' | LC_ALL=C sort)" == $'.SRCINFO\nPKGBUILD\nfixture-LICENSE' ]] || fail 'AUR extra whitelist incorrect'
printf 'Reuploaded license with the same version\n' > "$repo/packages/fixture-bin/fixture-LICENSE"
(cd "$repo" && git add packages/fixture-bin/fixture-LICENSE && git commit -q -m 'Changed source bytes without checksum' && git push -q origin HEAD:main)
source_commit="$(git -C "$repo" rev-parse HEAD)"
assert_fails prepare fixture-bin "$fixture/invalid-source-candidate"

# Publish may inspect candidate text but must never source it after prepare.
init_fixture
printf '\nprintf "ran\\n" > "%s"\n' "$fixture/sourced-marker" >> "$repo/packages/fixture-bin/PKGBUILD"
sed -i 's/^pkgrel=1$/pkgrel=2/' "$repo/packages/fixture-bin/PKGBUILD"
(cd "$repo/packages/fixture-bin" && makepkg --printsrcinfo > .SRCINFO)
(cd "$repo" && git add packages/fixture-bin && git commit -q -m 'Top-level metadata expression' && git push -q origin HEAD:main)
source_commit="$(git -C "$repo" rev-parse HEAD)"
set_upstream 1.3.0
prepare > /dev/null
rm -f "$fixture/sourced-marker"
publish > /dev/null
[[ ! -e "$fixture/sourced-marker" ]] || fail 'publish executed candidate PKGBUILD'

printf '%s\n' 'package release behavior: OK'
