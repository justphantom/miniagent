#!/bin/bash
set -euo pipefail

VERSION="${1:?用法: ./release.sh v3.2.5}"
TAG_MSG="${2:-Release $VERSION}"

# 检查是否为 annotated tag（杜绝 lightweight）
if [[ "$(git cat-file -t "$VERSION" 2>/dev/null || echo 'missing')" != "tag" ]]; then
    echo "错误: $VERSION 不是 annotated tag。用 git tag -a $VERSION -m '...' 创建" >&2
    exit 1
fi

# 检查工作树干净
if [[ -n "$(git status --porcelain)" ]]; then
    echo "错误: 工作树不干净，发版前请提交所有改动" >&2
    exit 1
fi

# 检查 Makefile 使用 --tags
if ! grep -q 'git describe.*--tags' Makefile; then
    echo "错误: Makefile 未使用 git describe --tags" >&2
    exit 1
fi

echo "✓ 发版检查通过: $VERSION"
echo "  git describe: $(git describe --tags --match "v*")"
echo "  类型: annotated tag"

# verify-gate：发版前全绿（AGENTS.md 五步：gofmt 空 / build ./... / vet / test -race / lint）
echo "→ 运行 verify-gate..."
make verify

# 构建
make build

echo "✓ 构建完成: bin/miniagent"
echo "  版本: $(./bin/miniagent -version)"
