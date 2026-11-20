#!/usr/bin/env bash
# Apply the verified Boxr worktree as focused feature commits to main.
# Run this from the root of an authenticated NetCore-Technologies/boxr clone.
set -euo pipefail

EXPECTED_REPO="NetCore-Technologies/boxr"

remote_matches_expected() {
  local remote_url="${1,,}"
  local expected_repo="${EXPECTED_REPO,,}"
  remote_url="${remote_url%.git}"
  remote_url="${remote_url%/}"

  case "$remote_url" in
    "https://github.com/${expected_repo}"|    "http://github.com/${expected_repo}"|    "ssh://git@github.com/${expected_repo}"|    "git@github.com:${expected_repo}"|    "git://github.com/${expected_repo}")
      return 0
      ;;
    *)
      return 1
      ;;
  esac
}

ROOT="$(git rev-parse --show-toplevel 2>/dev/null)" || {
  echo "error: run this script from inside the boxr Git checkout" >&2
  exit 1
}
cd "$ROOT"

BRANCH="$(git branch --show-current)"
if [[ "$BRANCH" != "main" ]]; then
  echo "error: this script only publishes to main (current branch: ${BRANCH:-detached})" >&2
  exit 1
fi

REMOTE_URL="$(git remote get-url origin)"
if ! remote_matches_expected "$REMOTE_URL"; then
  echo "error: origin does not point to ${EXPECTED_REPO}: ${REMOTE_URL}" >&2
  exit 1
fi

# Never build a commit stack on a stale local main. This avoids silently
# publishing against a baseline different from the one this work was tested on.
git fetch --quiet origin main
LOCAL_HEAD="$(git rev-parse HEAD)"
REMOTE_HEAD="$(git rev-parse FETCH_HEAD)"
if [[ "$LOCAL_HEAD" != "$REMOTE_HEAD" ]]; then
  echo "error: local main is not exactly origin/main" >&2
  echo "local:  ${LOCAL_HEAD}" >&2
  echo "remote: ${REMOTE_HEAD}" >&2
  echo "Run 'git pull --ff-only origin main', re-apply the worktree, then retry." >&2
  exit 1
fi

if ! git diff --cached --quiet; then
  echo "error: the index already contains staged changes; commit or unstage them first" >&2
  exit 1
fi

# Prevent this publishing helper from mixing unrelated local modifications.
allowed_paths=(
  .gitignore go.mod Makefile README.md CONTRIBUTING.md SECURITY.md LICENSE install.sh install.ps1
  cmd/boxr internal docs .devcontainer .github website scripts/push-to-main.sh
)
while IFS= read -r -d '' entry; do
  path="${entry:3}"
  allowed=false
  for allowed_path in "${allowed_paths[@]}"; do
    if [[ "$path" == "$allowed_path" || "$path" == "$allowed_path/"* ]]; then
      allowed=true
      break
    fi
  done
  if [[ "$allowed" != true ]]; then
    echo "error: unrelated working-tree change detected: $path" >&2
    exit 1
  fi
done < <(git status --porcelain --untracked-files=all -z)

command -v go >/dev/null || { echo "error: Go is required" >&2; exit 1; }
make verify
bash -n install.sh
bash -n scripts/push-to-main.sh

commit_group() {
  local message="$1"
  shift
  git add -- "$@"
  if ! git diff --cached --quiet; then
    git commit -m "$message"
  fi
}

commit_group "build: align Go toolchain and development verification" .gitignore go.mod Makefile .devcontainer scripts/push-to-main.sh
commit_group "feat(runtime): add namespace and cgroup execution controls" cmd/boxr internal/container
commit_group "feat(rootfs): add safe filesystem archives and integrity tools" internal/rootfs
commit_group "feat(workflows): add versioned config and container state lifecycle" internal/config internal/state
commit_group "feat(storage): add persistent named volumes and safe backups" internal/volume
commit_group "feat(images): add local OCI image-layout workflows" internal/image
commit_group "feat(registry): add verified OCI image pull and push" internal/registry
commit_group "ci: add race tests and security quality gates" .github/dependabot.yml .github/workflows/ci.yml .github/workflows/security.yml
commit_group "release: publish verified multi-architecture packages" .github/workflows/release.yml install.sh install.ps1
commit_group "docs: document runtime capabilities and security boundaries" README.md CONTRIBUTING.md SECURITY.md LICENSE docs
commit_group "site: publish Boxr product website" website .github/workflows/pages.yml

if [[ -n "$(git status --porcelain --untracked-files=all)" ]]; then
  echo "error: changes remain after the planned commit groups; refusing to push" >&2
  git status --short >&2
  exit 1
fi

echo "Created feature commits:"
git --no-pager log -11 --oneline
printf '\nPushing fast-forward only to origin/main...\n'
git push origin main
