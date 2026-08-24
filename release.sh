#!/bin/bash

# release.sh - Automated release tag script for MonsterMQ Edge
# Usage:
#   ./release.sh           # Auto-increments patch version (e.g. 0.1.7 -> 0.1.8)
#   ./release.sh 0.2.0     # Sets explicit version 0.2.0

set -e

RED='\033[0;31m'
GREEN='\033[0;32m'
YELLOW='\033[1;33m'
NC='\033[0m' # No Color

echo -e "${GREEN}=== MonsterMQ Edge Release Script ===${NC}"

if [ ! -f "version.txt" ]; then
    echo -e "${RED}Error: version.txt not found${NC}"
    exit 1
fi

CURRENT_VERSION=$(head -n 1 version.txt | tr -d '\n' | tr -d '\r')
BASE_VERSION=$(echo "$CURRENT_VERSION" | cut -d'+' -f1)

if [ -n "$1" ]; then
    NEW_VERSION="$1"
else
    IFS='.' read -r MAJOR MINOR PATCH <<< "$BASE_VERSION"
    if [ -z "$MAJOR" ] || [ -z "$MINOR" ] || [ -z "$PATCH" ]; then
        echo -e "${RED}Error: Invalid version format in version.txt. Expected format: X.Y.Z${NC}"
        echo -e "${RED}Current content: '$CURRENT_VERSION'${NC}"
        exit 1
    fi
    NEW_PATCH=$((PATCH + 1))
    NEW_VERSION="${MAJOR}.${MINOR}.${NEW_PATCH}"
fi

GIT_SHA=$(git rev-parse --short HEAD)

echo -e "${YELLOW}Current version : ${BASE_VERSION}${NC}"
echo -e "${GREEN}New version     : ${NEW_VERSION}${NC}"
echo -e "${GREEN}Git SHA         : ${GIT_SHA}${NC}"

if ! git diff-index --quiet HEAD --; then
    echo -e "${YELLOW}Warning: You have uncommitted changes${NC}"
    read -p "Do you want to continue? (y/n) " -n 1 -r
    echo
    if [[ ! $REPLY =~ ^[Yy]$ ]]; then
        echo -e "${RED}Release cancelled${NC}"
        exit 1
    fi
fi

if git rev-parse "v${NEW_VERSION}" >/dev/null 2>&1; then
    echo -e "${RED}Error: Tag v${NEW_VERSION} already exists${NC}"
    exit 1
fi

echo "$NEW_VERSION" > version.txt
echo -e "${GREEN}✓ Updated version.txt to ${NEW_VERSION}${NC}"

RELEASE_NOTES_FILE="releases/v${NEW_VERSION}.txt"
mkdir -p releases
echo "Release v${NEW_VERSION}" > "$RELEASE_NOTES_FILE"
echo "Built from commit: ${GIT_SHA}" >> "$RELEASE_NOTES_FILE"
echo "Date: $(date '+%Y-%m-%d %H:%M:%S')" >> "$RELEASE_NOTES_FILE"
echo "" >> "$RELEASE_NOTES_FILE"
echo "Changes since v${BASE_VERSION}:" >> "$RELEASE_NOTES_FILE"
echo "---" >> "$RELEASE_NOTES_FILE"

LAST_TAG=$(git describe --tags --abbrev=0 2>/dev/null || echo "")
if [ -n "$LAST_TAG" ]; then
    git log "${LAST_TAG}..HEAD" --oneline >> "$RELEASE_NOTES_FILE"
else
    echo "Initial release" >> "$RELEASE_NOTES_FILE"
fi

echo -e "${GREEN}✓ Created release notes: ${RELEASE_NOTES_FILE}${NC}"

git add version.txt "$RELEASE_NOTES_FILE"
git commit -m "Bump version to ${NEW_VERSION}" || {
    echo -e "${YELLOW}No changes to commit${NC}"
}

echo -e "${YELLOW}Creating tag v${NEW_VERSION}...${NC}"
git tag -a "v${NEW_VERSION}" -m "Release version ${NEW_VERSION}"
echo -e "${GREEN}✓ Created tag v${NEW_VERSION}${NC}"

echo -e "${YELLOW}Pushing commits and tags...${NC}"
git push
git push --tags
echo -e "${GREEN}✓ Pushed commits and tags${NC}"

echo ""
echo -e "${GREEN}=== Release Tag Complete ===${NC}"
echo -e "${GREEN}Version ${NEW_VERSION} tagged successfully.${NC}"
echo ""
echo -e "${YELLOW}Next steps:${NC}"
echo "  1. Build artifacts locally : ./build.sh"
echo "  2. Publish release assets  : ./publish.sh"
echo "  3. Or build & publish      : ./build.sh --publish"

