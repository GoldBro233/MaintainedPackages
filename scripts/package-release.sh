#!/usr/bin/env bash
set -euo pipefail

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd -P)"
config="$repo_root/packages.json"

fail() { printf '%s\n' "$*" >&2; exit 1; }
require_env() { [[ -n "${!1:-}" ]] || fail "Environment variable $1 is required"; }
git_root() { git -c "safe.directory=$repo_root" -C "$repo_root" "$@"; }

# Reject links in every component, including existing destination parents.
safe_path() {
  local path="$1" part current=""
  [[ "$path" == /* ]] || fail "Not an absolute path: $path"
  local -a parts
  IFS=/ read -r -a parts <<< "$path"
  for part in "${parts[@]}"; do
    [[ -z "$part" || "$part" == . || "$part" == .. ]] && { [[ -z "$part" ]] && continue; fail "Unsafe path: $path"; }
    current+="/$part"
    [[ ! -L "$current" ]] || fail "Symbolic link in path: $current"
  done
}
regular_file() { safe_path "$1"; [[ -f "$1" && ! -L "$1" ]] || fail "Missing regular file: $1"; }
outside_directory() {
  local path="$1" other="${2:-}"
  safe_path "$path"
  [[ "$path" != "$repo_root" && "$path" != "$repo_root/"* ]] || fail "Output inside checkout: $path"
  if [[ -n "$other" ]]; then
    [[ "$path" != "$other" && "$path" != "$other/"* && "$other" != "$path/"* ]] || fail "Overlapping directories"
  fi
  [[ ! -e "$path" || -d "$path" ]] || fail "Not a directory: $path"
  if [[ -d "$path" ]]; then
    [[ -z "$(find "$path" -mindepth 1 -print -quit)" ]] || fail "Directory is not empty: $path"
  fi
}

validate_config() {
  regular_file "$config"
  jq -e '
    type == "object" and (keys == ["packages"]) and (.packages | type == "array") and
    (.packages | all(.[];
      type == "object" and ((keys - ["aur_extra_files", "enabled", "name", "tag_prefix", "upstream_repo"]) == []) and
      (has("enabled") and (.enabled | type == "boolean")) and
      (.name | type == "string" and test("^[a-z0-9][a-z0-9@._+-]*$")) and
      (.upstream_repo | type == "string" and test("^[A-Za-z0-9]([A-Za-z0-9-]{0,37}[A-Za-z0-9])?/[A-Za-z0-9_.-]{1,100}$") and (split("/")[1] | . != "." and . != "..")) and
      (.tag_prefix | type == "string" and test("^[A-Za-z0-9._+-]*$")) and
      ((.aur_extra_files // []) | type == "array" and all(.[];
        type == "string" and length > 0 and (contains("\\") | not) and (test("[[:cntrl:]]") | not) and
        (startswith("/") | not) and (split("/") | all(.[]; . != "" and . != "." and . != ".." and . != ".git" and . != "src" and . != "pkg")) and
        . != "PKGBUILD" and . != ".SRCINFO"
      ) and (length == (unique | length)))
    )) and ([.packages[].name] | length == (unique | length))
  ' "$config" >/dev/null || fail "Invalid packages.json"
  local name path extra
  while IFS= read -r name; do
    for path in PKGBUILD .SRCINFO; do
      regular_file "$repo_root/packages/$name/$path"
    done
    while IFS= read -r extra; do
      path="$repo_root/packages/$name/$extra"
      safe_path "$path"
      if [[ -e "$path" || -L "$path" ]]; then regular_file "$path"; fi
    done < <(jq -r --arg name "$name" '.packages[] | select(.name == $name) | (.aur_extra_files // [])[]' "$config")
  done < <(jq -r '.packages[].name' "$config")
}

load_package_config() {
  local name="$1" record
  validate_config
  record="$(jq -ce --arg name "$name" '.packages[] | select(.name == $name)' "$config")" || fail "Unknown package: $name"
  [[ "$(jq -r '.enabled' <<< "$record")" == true ]] || fail "Disabled package: $name"
  package_name="$name"
  upstream_repo="$(jq -r '.upstream_repo' <<< "$record")"
  tag_prefix="$(jq -r '.tag_prefix' <<< "$record")"
  mapfile -t managed_files < <(jq -r '"PKGBUILD", ".SRCINFO", (.aur_extra_files // [])[]' <<< "$record")
}

select_matrix() {
  validate_config
  if (( $# )); then
    load_package_config "$1"
    jq -cn --arg name "$1" '{include:[{package:$name}]}'
  else
    jq -c '{include:[.packages[] | select(.enabled) | {package:.name}]}' "$config"
  fi
}

# Git blobs are read as data; link entries cannot become managed inputs.
blob_hash() {
  local repo="$1" ref="$2" path="$3" entry hash
  local -a git_cmd=(git -C "$repo")
  if [[ "$repo" == "$repo_root" ]]; then git_cmd=(git -c "safe.directory=$repo_root" -C "$repo"); fi
  entry="$("${git_cmd[@]}" ls-tree "$ref" -- "$path")" || fail "Cannot inspect $path"
  if [[ -z "$entry" ]]; then printf 'null\n'; return; fi
  [[ "$entry" == '100644 blob '*$'\t'"$path" || "$entry" == '100755 blob '*$'\t'"$path" ]] || fail "Non-regular Git entry: $path"
  hash="$("${git_cmd[@]}" show "$ref:$path" | sha256sum | cut -d' ' -f1)" || fail "Cannot read Git blob: $path"
  printf '%s\n' "$hash"
}
file_hash() {
  local file="$1"
  safe_path "$file"
  if [[ ! -e "$file" ]]; then printf 'null\n'; return; fi
  regular_file "$file"
  sha256sum "$file" | cut -d' ' -f1
}
source_version() {
  local file="$1" key value count_base=0 count_name=0 count_ver=0 count_rel=0 count_epoch=0
  local base="" pkg="" ver="" rel="" epoch=0
  regular_file "$file"
  while IFS= read -r line || [[ -n "$line" ]]; do
    if [[ "$line" =~ ^[[:space:]]*(pkgbase|pkgname|pkgver|pkgrel|epoch)[[:space:]]*=[[:space:]]*(.*)$ ]]; then
      key="${BASH_REMATCH[1]}" value="${BASH_REMATCH[2]}"
      case "$key" in
        pkgbase) base="$value"; ((++count_base)) ;;
        pkgname) pkg="$value"; ((++count_name)) ;;
        pkgver) ver="$value"; ((++count_ver)) ;;
        pkgrel) rel="$value"; ((++count_rel)) ;;
        epoch) epoch="$value"; ((++count_epoch)) ;;
      esac
    fi
  done < "$file"
  [[ "$count_base" == 1 && "$count_name" == 1 && "$count_ver" == 1 && "$count_rel" == 1 && "$count_epoch" -le 1 &&
     "$base" == "$package_name" && "$pkg" == "$package_name" &&
     "$ver" =~ ^[0-9]+(\.[0-9]+)*$ && "$rel" =~ ^[0-9]+(\.[0-9]+)*$ && "$epoch" =~ ^[0-9]+$ ]] || fail "Invalid single-package .SRCINFO: $file"
  src_pkgver="$ver"
  src_version="$ver-$rel"
  (( 10#$epoch == 0 )) || src_version="$epoch:$src_version"
}
metadata() {
  local directory="$1" output="$2"
  (cd "$directory" && makepkg --printsrcinfo) > "$output" || fail "Cannot read trusted package metadata"
  source_version "$output"
}
latest_version() {
  local tag response
  response="$(curl -fsSL "https://api.github.com/repos/$upstream_repo/releases/latest")" || fail "Cannot fetch upstream release"
  tag="$(jq -er '.tag_name | select(type == "string")' <<< "$response")" || fail "Missing upstream tag"
  [[ "$tag" == "$tag_prefix"* ]] || fail "Unexpected upstream tag prefix: $tag"
  latest="${tag#"$tag_prefix"}"
  [[ "$latest" =~ ^[0-9]+(\.[0-9]+)*$ ]] || fail "Unexpected upstream tag format: $tag"
}
compare_versions() {
  version_order="$(vercmp "$1" "$2")" || fail "Cannot compare versions: $1 and $2"
  [[ "$version_order" =~ ^-?[0-9]+$ ]] || fail "Invalid vercmp result"
}
update_package() {
  local dir="$1" ver="$2" key count
  for key in pkgver pkgrel; do
    count="$(sed -n "/^${key}=/p" "$dir/PKGBUILD" | wc -l)"
    [[ "$count" == 1 ]] || fail "Expected one static $key assignment"
  done
  sed -i -e "s/^pkgver=.*/pkgver=$ver/" -e 's/^pkgrel=.*/pkgrel=1/' "$dir/PKGBUILD"
  (cd "$dir" && updpkgsums) >&2 || fail "Cannot update source checksums"
}

# A successful empty clone must really have no refs. All other read failures stop publication.
aur_snapshot() {
  local dest="$1" refs
  git clone -q "https://aur.archlinux.org/$package_name.git" "$dest" || fail "Cannot read AUR repository"
  if aur_commit="$(git -C "$dest" rev-parse --verify refs/remotes/origin/master 2>/dev/null)"; then
    git -C "$dest" checkout -q -B master "$aur_commit" || fail "Cannot checkout AUR master"
    [[ "$(blob_hash "$dest" "$aur_commit" .SRCINFO)" != null ]] || fail "AUR .SRCINFO is missing"
  else
    refs="$(git -C "$dest" ls-remote origin)" || fail "Cannot read AUR refs"
    [[ -z "$refs" ]] || fail "AUR master is missing"
    aur_commit=null
  fi
}

json_hashes() {
  local kind="$1" repo="$2" ref="$3" prefix="$4" file hash result='{}'
  for file in "${managed_files[@]}"; do
    if [[ "$kind" == disk ]]; then hash="$(file_hash "$repo/$prefix$file")"
    elif [[ "$ref" == null ]]; then hash=null
    else hash="$(blob_hash "$repo" "$ref" "$prefix$file")"; fi
    if [[ "$hash" == null ]]; then result="$(jq -cn --argjson old "$result" --arg p "$file" '$old + {($p):null}')"
    else result="$(jq -cn --argjson old "$result" --arg p "$file" --arg h "$hash" '$old + {($p):$h}')"; fi
  done
  printf '%s\n' "$result"
}

validate_candidate() {
  local name="$1" dir="$2" expected actual manifest version source file files listed
  load_package_config "$name"
  safe_path "$dir"
  [[ "$dir" == /* && "$dir" != "$repo_root" && "$dir" != "$repo_root/"* ]] || fail "Unsafe candidate directory"
  [[ -d "$dir" && ! -L "$dir" ]] || fail "Missing candidate directory"
  regular_file "$dir/manifest.json"
  [[ -d "$dir/package" && ! -L "$dir/package" ]] || fail "Missing candidate package"
  [[ -z "$(find "$dir" -type l -print -quit)" ]] || fail "Candidate contains symbolic links"
  listed="$(find "$dir" -mindepth 1 -type f -printf '%P\n' | LC_ALL=C sort)"
  expected="$(printf 'manifest.json\n'; printf 'package/%s\n' "${managed_files[@]}")"
  [[ "$listed" == "$(LC_ALL=C sort <<< "$expected")" ]] || fail "Unexpected candidate files"
  local actual_dirs expected_dirs='package'
  for file in "${managed_files[@]}"; do
    local parent="${file%/*}"
    if [[ "$parent" != "$file" ]]; then
      while [[ "$parent" != . ]]; do
        expected_dirs+=$'\n'"package/$parent"
        [[ "$parent" == */* ]] || break
        parent="${parent%/*}"
      done
    fi
  done
  actual_dirs="$(find "$dir" -mindepth 1 -type d -printf '%P\n' | LC_ALL=C sort -u)"
  [[ "$actual_dirs" == "$(LC_ALL=C sort -u <<< "$expected_dirs")" ]] || fail "Unexpected candidate directories"
  [[ -z "$(find "$dir" -mindepth 1 ! -type f ! -type d -print -quit)" ]] || fail "Unexpected candidate entry"
  manifest="$dir/manifest.json"
  jq -e --arg name "$name" '
    type == "object" and (keys == ["aur_commit","aur_files","files","package","source_commit","version"]) and
    .package == $name and (.source_commit | type == "string" and test("^[0-9a-f]{40}$")) and
    (.aur_commit == null or (.aur_commit | type == "string" and test("^[0-9a-f]{40}$"))) and
    (.version | type == "string" and test("^([0-9]+:)?[0-9]+(\\.[0-9]+)*-[0-9]+(\\.[0-9]+)*$")) and
    (.files | type == "object" and all(.[]; type == "string" and test("^[0-9a-f]{64}$"))) and
    (.aur_files | type == "object" and all(.[]; . == null or (type == "string" and test("^[0-9a-f]{64}$"))))
  ' "$manifest" >/dev/null || fail "Invalid candidate manifest"
  source="$(jq -r '.source_commit' "$manifest")"
  [[ -z "${SOURCE_COMMIT:-}" || "$SOURCE_COMMIT" == "$source" ]] || fail "Candidate source commit mismatch"
  [[ "$source" == "$(git_root rev-parse HEAD)" ]] || fail "Candidate does not match trusted checkout"
  files="$(json_hashes disk "$dir" null 'package/')"
  actual="$(jq -c '.files' "$manifest")"
  [[ "$(jq -Sc . <<< "$files")" == "$(jq -Sc . <<< "$actual")" ]] || fail "Candidate file hashes differ"
  expected="$(printf '%s\n' "${managed_files[@]}" | jq -R . | jq -sc .)"
  jq -e --argjson names "$expected" '(.files | keys) == ($names | sort) and (.aur_files | keys) == ($names | sort)' "$manifest" >/dev/null || fail "Candidate file set differs"
  source_version "$dir/package/.SRCINFO"
  version="$(jq -r '.version' "$manifest")"
  [[ "$src_version" == "$version" ]] || fail "Candidate version differs from .SRCINFO"
}

prepare_candidate() {
  local name="$1" dir="$2" temp source current_pkgver version aur_files files aur_diff repo_diff force file hash need_verify=false
  load_package_config "$name"
  outside_directory "$dir"
  force="${FORCE_BUILD:-false}"
  [[ "$force" == true || "$force" == false ]] || fail "FORCE_BUILD must be true or false"
  source="$(git_root rev-parse HEAD)" || fail "Cannot read checkout HEAD"
  [[ -z "${SOURCE_COMMIT:-}" || "$SOURCE_COMMIT" == "$source" ]] || fail "SOURCE_COMMIT is not checkout HEAD"
  temp="$(mktemp -d)"
  release_temp="$temp"
  trap 'rm -rf -- "$release_temp"' EXIT
  mkdir -p "$temp/trusted" "$dir/package"
  for file in PKGBUILD .SRCINFO; do
    [[ "$(blob_hash "$repo_root" "$source" "packages/$name/$file")" != null ]] || fail "Missing tracked source: $file"
    git_root show "$source:packages/$name/$file" > "$temp/trusted/$file" || fail "Cannot copy trusted input"
  done
  metadata "$temp/trusted" "$temp/current.SRCINFO"
  current_pkgver="$src_pkgver"
  for file in "${managed_files[@]:2}"; do
    hash="$(blob_hash "$repo_root" "$source" "packages/$name/$file")"
    if [[ "$hash" != null ]] && ! grep -Fq "source = $file::" "$temp/current.SRCINFO"; then
      mkdir -p "$temp/trusted/$(dirname "$file")"
      git_root show "$source:packages/$name/$file" > "$temp/trusted/$file" || fail "Cannot copy local source"
    fi
  done
  latest_version
  compare_versions "$latest" "$current_pkgver"
  if (( version_order > 0 )); then
    update_package "$temp/trusted" "$latest"
    need_verify=true
  fi
  metadata "$temp/trusted" "$dir/package/.SRCINFO"
  version="$src_version"
  cp "$temp/trusted/PKGBUILD" "$dir/package/PKGBUILD"
  aur_snapshot "$temp/aur"
  aur_files="$(json_hashes git "$temp/aur" "$aur_commit" '')"
  if [[ "$aur_commit" != null ]]; then
    git -C "$temp/aur" show "$aur_commit:.SRCINFO" > "$temp/aur.SRCINFO" || fail "Cannot read AUR version"
    source_version "$temp/aur.SRCINFO"
    compare_versions "$src_version" "$version"
    (( version_order <= 0 )) || fail "AUR version is newer than candidate"
    if [[ "$src_version" == "$version" && "$(blob_hash "$temp/aur" "$aur_commit" PKGBUILD)" != "$(file_hash "$dir/package/PKGBUILD")" ]]; then
      fail "Same version has different PKGBUILD; increase pkgrel"
    fi
  fi
  for file in "${managed_files[@]:2}"; do
    if [[ -f "$temp/trusted/$file" ]]; then
      mkdir -p "$dir/package/$(dirname "$file")"
      cp -- "$temp/trusted/$file" "$dir/package/$file"
    else
      need_verify=true
    fi
  done
  if [[ "$need_verify" == true || ${#managed_files[@]} -gt 2 ]]; then
    (cd "$temp/trusted" && makepkg --verifysource --noconfirm) >&2 || fail "Source verification failed"
  fi
  for file in "${managed_files[@]:2}"; do
    if [[ ! -e "$dir/package/$file" ]]; then
      regular_file "$temp/trusted/$file"
      mkdir -p "$dir/package/$(dirname "$file")"
      cp -- "$temp/trusted/$file" "$dir/package/$file"
    fi
  done
  files="$(json_hashes disk "$dir" null 'package/')"
  repo_diff=false aur_diff=false
  for file in PKGBUILD .SRCINFO; do
    [[ "$(blob_hash "$repo_root" "$source" "packages/$name/$file")" == "$(file_hash "$dir/package/$file")" ]] || repo_diff=true
  done
  [[ "$(jq -Sc . <<< "$files")" == "$(jq -Sc . <<< "$aur_files")" ]] || aur_diff=true
  jq -cn --arg package "$name" --arg source_commit "$source" --arg version "$version" --arg aur_commit "$aur_commit" --argjson aur_files "$aur_files" --argjson files "$files" \
    '{package:$package,source_commit:$source_commit,version:$version,aur_commit:(if $aur_commit == "null" then null else $aur_commit end),aur_files:$aur_files,files:$files}' > "$dir/manifest.json"
  validate_candidate "$name" "$dir"
  if [[ "$repo_diff" == true || "$aur_diff" == true || "$force" == true ]]; then
    if [[ "$need_verify" != true && ${#managed_files[@]} -eq 2 ]]; then
      (cd "$temp/trusted" && makepkg --verifysource --noconfirm) >&2 || fail "Source verification failed"
    fi
    printf 'needs_build=true\n'
  else
    printf 'needs_build=false\n'
  fi
  if [[ "$repo_diff" == true || "$aur_diff" == true ]]; then printf 'needs_publish=true\n'
  else printf 'needs_publish=false\n'; fi
  printf 'candidate_version=%s\n' "$version"
}

collect_dependency_names() {
  local dir="$1" arch key value name
  arch="${CARCH:-$(uname -m)}"
  source_version "$dir/package/.SRCINFO"
  local -A seen=()
  while IFS= read -r line || [[ -n "$line" ]]; do
    if [[ "$line" =~ ^[[:space:]]*(depends|makedepends|checkdepends)(_${arch})?[[:space:]]*=[[:space:]]*(.*)$ ]]; then
      value="${BASH_REMATCH[3]}"
      name="${value%%[<>=]*}"
      [[ "$name" =~ ^[a-zA-Z0-9@._+-]+$ ]] || fail "Invalid dependency: $value"
      if [[ -z "${seen[$name]:-}" ]]; then printf '%s\n' "$name"; seen[$name]=1; fi
    fi
  done < "$dir/package/.SRCINFO"
}
install_build_deps() {
  local dir="$2" output
  require_env SOURCE_COMMIT
  validate_candidate "$1" "$dir"
  output="$(collect_dependency_names "$dir")" || fail "Cannot parse candidate dependencies"
  local -a deps=()
  if [[ -n "$output" ]]; then mapfile -t deps <<< "$output"; fi
  if (( ${#deps[@]} )); then pacman -S --noconfirm --needed "${deps[@]}" || fail "pacman failed"; fi
}
build_package() {
  local dir="$2" build="$3" file
  require_env SOURCE_COMMIT
  validate_candidate "$1" "$dir"
  outside_directory "$build" "$dir"
  mkdir -p "$build"
  for file in "${managed_files[@]}"; do
    mkdir -p "$build/$(dirname "$file")"
    cp -- "$dir/package/$file" "$build/$file"
  done
  (cd "$build" && makepkg -f --noconfirm) || fail "Build failed"
}

configure_ssh() {
  local temp="$1" known="$repo_root/.github/aur_known_hosts" line
  require_env AUR_SSH_PRIVATE_KEY
  regular_file "$known"
  [[ "$(wc -l < "$known")" == 1 ]] || fail "Invalid AUR host key file"
  line="$(< "$known")"
  [[ "$line" =~ ^aur\.archlinux\.org[[:space:]]ssh-ed25519[[:space:]]([A-Za-z0-9+/]+={0,2})$ ]] || fail "Invalid AUR host key record"
  ssh-keygen -lf "$known" -E sha256 >/dev/null 2>&1 || fail "Invalid AUR ed25519 key"
  install -d -m 700 "$temp/ssh"
  printf '%s\n' "$AUR_SSH_PRIVATE_KEY" > "$temp/ssh/key"
  cp "$known" "$temp/ssh/known_hosts"
  chmod 600 "$temp/ssh/key" "$temp/ssh/known_hosts"
  printf 'Host aur.archlinux.org\n  BatchMode yes\n  StrictHostKeyChecking yes\n  IdentitiesOnly yes\n  IdentityAgent none\n  UserKnownHostsFile %s\n  GlobalKnownHostsFile /dev/null\n  IdentityFile %s\n' "$temp/ssh/known_hosts" "$temp/ssh/key" > "$temp/ssh/config"
  chmod 600 "$temp/ssh/config"
}

# Only a concurrent non-fast-forward is retryable; authorization and transport failures are not.
retryable_push() {
  local output="$1"
  [[ "$output" == *'[rejected] (non-fast-forward)'* || "$output" == *'[rejected] (fetch first)'* ]]
}
check_aur() {
  local repo="$1" baseline="$2" candidate="$3" current version file actual expected
  current="$(git -C "$repo" rev-parse --verify refs/remotes/origin/master 2>/dev/null)" || current=null
  actual="$(json_hashes git "$repo" "$current" '')"
  expected="$(jq -c '.files' "$candidate/manifest.json")"
  [[ "$(jq -Sc . <<< "$actual")" != "$(jq -Sc . <<< "$expected")" ]] || return 1
  [[ "$(jq -Sc . <<< "$actual")" == "$(jq -Sc . <<< "$baseline")" ]] || fail "AUR managed files changed since prepare"
  if [[ "$current" != null ]]; then
    [[ "$(blob_hash "$repo" "$current" .SRCINFO)" != null ]] || fail "AUR .SRCINFO is missing"
    git -C "$repo" show "$current:.SRCINFO" > "$repo/.candidate-aur-version" || fail "Cannot read AUR metadata"
    source_version "$repo/.candidate-aur-version"
    version="$(jq -r '.version' "$candidate/manifest.json")"
    compare_versions "$src_version" "$version"
    (( version_order <= 0 )) || fail "AUR version is newer than candidate"
    [[ "$src_version" != "$version" || "$(blob_hash "$repo" "$current" PKGBUILD)" == "$(file_hash "$candidate/package/PKGBUILD")" ]] || fail "Same version has different PKGBUILD; increase pkgrel"
  fi
  return 0
}
check_github() {
  local repo="$1" ref="$2" candidate="$3" source file path initial current wanted selected
  source="$(jq -r '.source_commit' "$candidate/manifest.json")"
  git -C "$repo" merge-base --is-ancestor "$source" "$ref" || fail "Source commit is not an ancestor of GitHub target"
  for file in packages.json scripts/package-release.sh .github/aur_known_hosts .github/workflows/maintain-packages.yml .github/workflows/release-package.yml; do
    initial="$(blob_hash "$repo_root" "$source" "$file")"
    current="$(blob_hash "$repo" "$ref" "$file")"
    [[ "$initial" == "$current" ]] || fail "Trusted maintenance configuration changed: $file"
  done
  initial="$(git_root show "$source:packages.json" | jq -c --arg name "$package_name" '.packages[] | select(.name == $name)')" || fail "Cannot read source configuration"
  selected="$(git -C "$repo" show "$ref:packages.json" | jq -c --arg name "$package_name" '.packages[] | select(.name == $name)')" || fail "Cannot read target configuration"
  [[ -n "$selected" && "$initial" == "$selected" && "$(jq -r '.enabled' <<< "$selected")" == true ]] || fail "Package configuration changed or disabled on GitHub"
  for file in "${managed_files[@]}"; do
    path="packages/$package_name/$file"
    initial="$(blob_hash "$repo_root" "$source" "$path")"
    current="$(blob_hash "$repo" "$ref" "$path")"
    wanted="$(file_hash "$candidate/package/$file")"
    [[ "$current" == "$initial" || "$current" == "$wanted" ]] || fail "GitHub package changed since prepare: $path"
  done
}

publish_candidate() {
  local name="$1" candidate="$2" temp baseline aur_repo gh_repo gh_url ref file output attempt aur_synced=false
  require_env SOURCE_COMMIT
  require_env GITHUB_TARGET_BRANCH
  [[ "$GITHUB_TARGET_BRANCH" == main ]] || fail "Publication must target main"
  validate_candidate "$name" "$candidate"
  temp="$(mktemp -d)"
  release_temp="$temp"
  trap 'rm -rf -- "$release_temp"' EXIT
  baseline="$(jq -c '.aur_files' "$candidate/manifest.json")"
  gh_url="$(git_root remote get-url origin)" || fail "Cannot determine trusted GitHub origin"
  git clone -q "$gh_url" "$temp/github" || fail "Cannot read GitHub origin"
  gh_repo="$temp/github"
  git -C "$gh_repo" fetch -q origin "$GITHUB_TARGET_BRANCH" || fail "Cannot fetch GitHub branch"
  ref="$(git -C "$gh_repo" rev-parse --verify "refs/remotes/origin/$GITHUB_TARGET_BRANCH")" || fail "Missing GitHub branch"
  check_github "$gh_repo" "$ref" "$candidate"
  for ((attempt=1; attempt<=3; attempt++)); do
    aur_repo="$temp/aur-$attempt"
    aur_snapshot "$aur_repo"
    if ! check_aur "$aur_repo" "$baseline" "$candidate"; then aur_synced=true; break; fi
    if [[ "$aur_commit" == null ]]; then git -C "$aur_repo" checkout -q -b master || fail "Cannot create AUR master"; fi
    configure_ssh "$temp"
    for file in "${managed_files[@]}"; do
      safe_path "$aur_repo/$file"
      mkdir -p "$aur_repo/$(dirname "$file")"
      cp -- "$candidate/package/$file" "$aur_repo/$file"
    done
    git -C "$aur_repo" config user.name 'github-actions[bot]'
    git -C "$aur_repo" config user.email '41898282+github-actions[bot]@users.noreply.github.com'
    git -C "$aur_repo" add -- "${managed_files[@]}"
    git -C "$aur_repo" commit -qm "chore($name): update to $(jq -r '.version' "$candidate/manifest.json")" || fail "Cannot commit AUR metadata"
    git -C "$aur_repo" remote set-url --push origin "ssh://aur@aur.archlinux.org/$name.git"
    if output="$(LC_ALL=C GIT_SSH_COMMAND="ssh -F $temp/ssh/config" git -C "$aur_repo" push --porcelain origin HEAD:master 2>&1)"; then aur_synced=true; break; fi
    if ! retryable_push "$output"; then printf '%s\n' "$output" >&2; fail "AUR push failed"; fi
    printf 'AUR changed concurrently, retrying (%s/3)\n' "$attempt" >&2
  done
  [[ "$aur_synced" == true ]] || fail "AUR push rejected after 3 attempts"
  for ((attempt=1; attempt<=3; attempt++)); do
    if ! git -C "$gh_repo" fetch -q origin "$GITHUB_TARGET_BRANCH"; then fail "AUR synchronized; GitHub synchronization failed: fetch"; fi
    ref="$(git -C "$gh_repo" rev-parse --verify "refs/remotes/origin/$GITHUB_TARGET_BRANCH")" || fail "AUR synchronized; GitHub synchronization failed: missing branch"
    if ! (check_github "$gh_repo" "$ref" "$candidate"); then fail "AUR synchronized; GitHub synchronization failed: target changed"; fi
    git -C "$gh_repo" checkout -q -B maintenance "$ref" || fail "AUR synchronized; GitHub synchronization failed: checkout"
    for file in PKGBUILD .SRCINFO; do
      cp -- "$candidate/package/$file" "$gh_repo/packages/$name/$file"
    done
    git -C "$gh_repo" add -- "packages/$name/PKGBUILD" "packages/$name/.SRCINFO"
    if git -C "$gh_repo" diff --cached --quiet; then return; fi
    git -C "$gh_repo" config user.name 'github-actions[bot]'
    git -C "$gh_repo" config user.email '41898282+github-actions[bot]@users.noreply.github.com'
    git -C "$gh_repo" commit -qm "chore($name): update to $(jq -r '.version' "$candidate/manifest.json")" || fail "AUR synchronized; GitHub synchronization failed: commit"
    require_env GITHUB_TOKEN
    local auth
    auth="$(printf 'x-access-token:%s' "$GITHUB_TOKEN" | base64 -w0)"
    if output="$(LC_ALL=C GIT_CONFIG_COUNT=1 GIT_CONFIG_KEY_0=http.https://github.com/.extraheader GIT_CONFIG_VALUE_0="AUTHORIZATION: basic $auth" git -C "$gh_repo" push --porcelain origin HEAD:"$GITHUB_TARGET_BRANCH" 2>&1)"; then return; fi
    if ! retryable_push "$output"; then printf '%s\n' "$output" >&2; fail "AUR synchronized; GitHub synchronization failed: push"; fi
    printf 'GitHub changed concurrently, retrying (%s/3)\n' "$attempt" >&2
  done
  fail "AUR synchronized; GitHub synchronization failed: concurrent pushes"
}

main() {
  local command="${1:-}" name="${2:-}"
  case "$command" in
    matrix) (( $# <= 2 )) || fail 'Usage: matrix [name]'; select_matrix "${@:2}" ;;
    prepare) (( $# == 3 )) || fail 'Usage: prepare <name> <candidate-dir>'; prepare_candidate "$name" "$3" ;;
    install-build-deps) (( $# == 3 )) || fail 'Usage: install-build-deps <name> <candidate-dir>'; install_build_deps "$name" "$3" ;;
    build) (( $# == 4 )) || fail 'Usage: build <name> <candidate-dir> <build-dir>'; build_package "$name" "$3" "$4" ;;
    publish) (( $# == 3 )) || fail 'Usage: publish <name> <candidate-dir>'; publish_candidate "$name" "$3" ;;
    *) fail 'Usage: package-release.sh <matrix|prepare|install-build-deps|build|publish> ...' ;;
  esac
}
main "$@"
