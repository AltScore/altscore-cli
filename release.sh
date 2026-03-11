#!/usr/bin/env bash
set -euo pipefail

if [[ $# -ne 1 ]]; then
    echo "Usage: $0 <version>"
    echo "Example: $0 v0.1.0"
    exit 1
fi

VERSION="$1"

if [[ ! "$VERSION" =~ ^v ]]; then
    echo "Error: version must start with 'v' (e.g. v0.1.0)"
    exit 1
fi

for cmd in go gh; do
    if ! command -v "$cmd" &>/dev/null; then
        echo "Error: '$cmd' is not installed"
        exit 1
    fi
done

if [[ -n "$(git status --porcelain)" ]]; then
    echo "Error: working tree is not clean. Commit or stash changes first."
    exit 1
fi

# Tag (skip if already exists)
if git rev-parse "$VERSION" &>/dev/null; then
    echo "Tag $VERSION already exists, skipping tag creation"
else
    echo "Creating tag $VERSION"
    git tag -a "$VERSION" -m "Release $VERSION"
fi

# Push tag (skip if already on remote)
if git ls-remote --tags origin "$VERSION" | grep -q "$VERSION"; then
    echo "Tag $VERSION already on remote, skipping push"
else
    echo "Pushing tag $VERSION to origin"
    git push origin "$VERSION"
fi

DIST_DIR=$(mktemp -d)
trap 'rm -rf "$DIST_DIR"' EXIT

PLATFORMS=("darwin/arm64" "darwin/amd64" "linux/amd64")

for platform in "${PLATFORMS[@]}"; do
    GOOS="${platform%/*}"
    GOARCH="${platform#*/}"
    OUTPUT="altscore-${GOOS}-${GOARCH}"
    echo "Building $OUTPUT"
    CGO_ENABLED=0 GOOS="$GOOS" GOARCH="$GOARCH" \
        go build -buildvcs=false \
        -ldflags="-s -w -X github.com/AltScore/altscore-cli/internal/version.Version=$VERSION" \
        -o "$DIST_DIR/$OUTPUT" .
done

echo "Generating checksums"
(cd "$DIST_DIR" && shasum -a 256 altscore-* > checksums.txt)

echo "Creating GitHub release $VERSION"
gh release create "$VERSION" "$DIST_DIR"/* \
    --title "$VERSION" \
    --generate-notes

echo "Release $VERSION published"
