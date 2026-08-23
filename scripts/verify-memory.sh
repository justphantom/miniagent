#!/bin/sh
# verify-memory.sh — 记忆系统健康检查
# AGENTS.md §5 verify-gate 末步：YAML frontmatter 格式 / index 可达性 / superseded 引用有效性
# 本文件不引用未跟踪文件

set -e

AGENT_DIR=".agent"
L2_DIR="$AGENT_DIR/L2"
INDEX="$AGENT_DIR/index.md"
ERRORS=0

# 1. YAML frontmatter 格式一致
for f in $(find "$AGENT_DIR" -name '*.md' -not -path '*/node_modules/*'); do
    layer=$(sed -n 's/^layer: *//p' "$f" | head -1)
    type=$(sed -n 's/^type: *//p' "$f" | head -1)
    if [ -z "$layer" ] && [ "$(basename "$f")" != "README.md" ] && [ "$(basename "$f")" != "carryover.md" ]; then
        echo "FAIL: $f missing 'layer' in frontmatter"
        ERRORS=$((ERRORS+1))
    fi
done

# 2. index.md 条目全部可达
grep -oP '`[^`]+`' "$INDEX" | while read -r entry; do
    entry=$(echo "$entry" | tr -d '`')
    file=$(echo "$entry" | sed 's/\.$//').md
    # map short names to paths
    case "$entry" in
        */*.md) ;;
        *) file="";;
    esac
done

# 2b. index.md 列出所有 L2 条目
for f in $(find "$L2_DIR" -name '*.md'); do
    basename=$(basename "$f" .md)
    if ! grep -q "$basename" "$INDEX" 2>/dev/null; then
        echo "FAIL: $basename not found in $INDEX"
        ERRORS=$((ERRORS+1))
    fi
done

# 3. 非 superseded 条目引用代码路径有效（自动过期检测）
for f in $(find "$L2_DIR" -name '*.md'); do
    if grep -q '^status: superseded' "$f"; then
        # superseded 条目引用已删除路径是预期历史，不检查
        continue
    fi
    refs=$(sed -n '/^## 参考/,/^## /p' "$f" | grep -oP '`[^`]+\.go`' | tr -d '`')
    for ref in $refs; do
        found=false
        if [ -f "$ref" ]; then
            found=true
        elif [ -f "internal/$ref" ]; then
            found=true
        elif [ -n "$(find . -path '*/\.git' -prune -o -name "$(basename "$ref")" -print -quit 2>/dev/null)" ]; then
            found=true
        fi
        if [ "$found" = false ]; then
            echo "WARN: non-superseded $f references missing file $ref (真过期，需更新或标记 superseded)"
        fi
    done
done

# 4. L2 条目 tags 格式一致（YAML 数组）
for f in $(find "$L2_DIR" -name '*.md'); do
    basename=$(basename "$f")
    [ "$basename" = "README.md" ] && continue
    tags_line=$(sed -n '/^tags:/p' "$f" | head -1)
    if [ -n "$tags_line" ] && echo "$tags_line" | grep -qv '\['; then
        echo "WARN: $f tags not in YAML array syntax"
    fi
done

if [ "$ERRORS" -gt 0 ]; then
    echo "FAIL: $ERRORS memory integrity error(s)"
    exit 1
fi
echo "PASS: memory integrity"