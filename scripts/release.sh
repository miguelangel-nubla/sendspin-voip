#!/usr/bin/env bash
set -euo pipefail

# Ensure we are in repo root
REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$REPO_ROOT"

CONFIG_FILE="sendspin-voip/config.yaml"
CHANGELOG_FILE="sendspin-voip/CHANGELOG.md"

# 1. Determine current latest tag
LATEST_TAG="$(git describe --tags --abbrev=0 2>/dev/null || echo "v1.0.0")"
LATEST_VER="${LATEST_TAG#v}"

IFS='.' read -r MAJOR MINOR PATCH <<< "$LATEST_VER"
MAJOR="${MAJOR:-1}"
MINOR="${MINOR:-0}"
PATCH="${PATCH:-0}"

# 2. Determine target version
TARGET_INPUT="${1:-patch}"

case "$TARGET_INPUT" in
  patch)
    NEXT_PATCH=$((PATCH + 1))
    NEXT_VER="${MAJOR}.${MINOR}.${NEXT_PATCH}"
    ;;
  minor)
    NEXT_MINOR=$((MINOR + 1))
    NEXT_VER="${MAJOR}.${NEXT_MINOR}.0"
    ;;
  major)
    NEXT_MAJOR=$((MAJOR + 1))
    NEXT_VER="${NEXT_MAJOR}.0.0"
    ;;
  v*|*.*.*)
    NEXT_VER="${TARGET_INPUT#v}"
    ;;
  *)
    echo "Usage: $0 [patch|minor|major|vX.Y.Z]" >&2
    exit 1
    ;;
esac

NEXT_TAG="v${NEXT_VER}"
echo "==> Releasing ${NEXT_TAG} (previous was ${LATEST_TAG})"

# 3. Collect commits since last tag for changelog
COMMITS="$(git log "${LATEST_TAG}..HEAD" --no-merges --pretty=format:"- %s" 2>/dev/null || true)"
if [ -z "$COMMITS" ]; then
  COMMITS="- Release ${NEXT_TAG}"
fi

# 4. Update sendspin-voip/config.yaml
if [ -f "$CONFIG_FILE" ]; then
  echo "==> Updating ${CONFIG_FILE} version to ${NEXT_VER}"
  python3 -c '
import sys, re
path, next_ver = sys.argv[1], sys.argv[2]
with open(path, "r", encoding="utf-8") as f:
    content = f.read()
new_content = re.sub(r"(?m)^version:\s*[\"'\''].*?[\"'\''].*", f"version: \"{next_ver}\"", content)
with open(path, "w", encoding="utf-8") as f:
    f.write(new_content)
' "$CONFIG_FILE" "$NEXT_VER"
fi

# 5. Update sendspin-voip/CHANGELOG.md
if [ -f "$CHANGELOG_FILE" ]; then
  echo "==> Updating ${CHANGELOG_FILE}"
  python3 -c '
import sys
path, next_ver, commits = sys.argv[1], sys.argv[2], sys.argv[3]
with open(path, "r", encoding="utf-8") as f:
    content = f.read()

header = f"## {next_ver}"
if header not in content:
    section = f"## {next_ver}\n\n{commits}\n\n"
    if content.startswith("# Changelog\n\n"):
        new_content = "# Changelog\n\n" + section + content[len("# Changelog\n\n"):]
    elif content.startswith("# Changelog\n"):
        new_content = "# Changelog\n\n" + section + content[len("# Changelog\n"):]
    else:
        new_content = "# Changelog\n\n" + section + content
    with open(path, "w", encoding="utf-8") as f:
        f.write(new_content)
' "$CHANGELOG_FILE" "$NEXT_VER" "$COMMITS"
fi

# 6. Verify codebase
echo "==> Running checks and tests..."
make check

# 7. Git commit and tag
echo "==> Creating release commit and git tag..."
git add -A
if ! git diff --cached --quiet; then
  git commit -m "chore: bump version to ${NEXT_VER} and update changelog"
fi

if git rev-parse "$NEXT_TAG" >/dev/null 2>&1; then
  echo "Tag ${NEXT_TAG} already exists."
else
  git tag -a "$NEXT_TAG" -m "Release ${NEXT_TAG}"
  echo "==> Tagged ${NEXT_TAG}"
fi

echo ""
echo "Release ${NEXT_TAG} prepared successfully!"
echo "To publish, run:"
echo "  git push origin main --follow-tags"
