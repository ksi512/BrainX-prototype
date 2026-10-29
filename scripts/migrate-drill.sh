#!/usr/bin/env sh
# 迁移演练：验证「换机器 / 换用户名 / 换父目录」之后一切引用仍可解析。
#
# 为什么必须含「换用户名」这一条：库内只要存了一条绝对路径，
# 换用户名之后它只在取原件那一刻失败，报的还是「文件不存在」——
# 这是全套硬约束里最难排查的一种静默失效。
set -eu

brainx_bin=${1:-./brainx}
drill_root=$(mktemp -d)
alice_home="$drill_root/user-alice/brainx"
bob_home="$drill_root/user-bob/brainx"
trap 'rm -rf "$drill_root"' EXIT

fail() {
	printf 'FAIL: %s\n' "$1" >&2
	exit 1
}

# ── 1. 在 alice 的家目录下建库，导入一份带原件的文档 ──
BRAINX_HOME="$alice_home" "$brainx_bin" init >/dev/null
printf '# 迁移演练文档\n\n这是一份用于验证原件相对路径的文档，正文足够长以便入库。\n' > "$drill_root/paper.md"
doc_id=$(BRAINX_HOME="$alice_home" "$brainx_bin" import "$drill_root/paper.md" --scope global --kind doc)
BRAINX_HOME="$alice_home" "$brainx_bin" remember "迁移演练：部署用 compose，数据目录在 /vol2/1000/docker" --kind fact --scope global >/dev/null

# ── 2. 库内不得出现绝对路径（写入即断言，不等 doctor） ──
bad=$(sqlite3 "$alice_home/store.db" "SELECT COUNT(*) FROM assets WHERE archive LIKE '/%' OR archive LIKE '~%' OR origin LIKE 'file:/%' OR origin LIKE 'file:~%'")
[ "$bad" = "0" ] || fail "库内有 $bad 条绝对路径引用"

# ── 3. 拷贝到另一个用户名 + 另一个父目录（模拟换机器） ──
mkdir -p "$(dirname "$bob_home")"
cp -r "$alice_home" "$bob_home"

# ── 4. 新家必须能检索、能取原件、体检全过 ──
BRAINX_HOME="$bob_home" "$brainx_bin" recall "迁移" --scope global | grep -q "compose" || fail "换家之后召回失败"

opened=$(BRAINX_HOME="$bob_home" "$brainx_bin" get "$doc_id" --open)
case "$opened" in
"$bob_home"/*) : ;;
*) fail "原件路径没有跟着新家走：$opened" ;;
esac
[ -f "$opened" ] || fail "原件在新家里不存在：$opened"

BRAINX_HOME="$bob_home" "$brainx_bin" doctor >/dev/null || fail "新家体检未通过"

# ── 5. export 必须是相对路径打包，且能独立解开 ──
BRAINX_HOME="$bob_home" "$brainx_bin" export "$drill_root/brainx.mbz"
if unzip -l "$drill_root/brainx.mbz" | awk '{print $4}' | grep -qE '^/'; then
	fail "export 包里出现了绝对路径条目"
fi

printf 'migration drill passed\n  alice: %s\n  bob:   %s\n  opened: %s\n' "$alice_home" "$bob_home" "$opened"
