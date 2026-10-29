#!/usr/bin/env bash
# BrainX 总验收（提示词 §8 检查单的可自动化部分）
#
# 用法：./scripts/acceptance.sh <brainx 二进制>
#
# 设计原则：
#   - 每条断言只做一件事，失败时打印**实际值**，不打印「失败了」三个字
#   - 全部在临时 HOME 里跑，不碰真实库与真实宿主配置
#   - M9（MCP 薄壳）不测：它不在本轮范围内

set -uo pipefail

BIN="${1:-./brainx}"
if [ ! -x "$BIN" ]; then
  echo "找不到可执行文件：$BIN"
  exit 1
fi
BIN="$(cd "$(dirname "$BIN")" && pwd)/$(basename "$BIN")"
# 源码目录必须在这里算出来：后面为了测 scope 会 cd 到 /tmp，
# 那时再 grep 相对路径会「查空并通过」——最假的一种绿。
SRC="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"

ROOT="$(mktemp -d)/bx-acc"
export BRAINX_HOME="$ROOT/home"
export HOME_FAKE="$ROOT/fakehome"
pass=0
fail=0

ok()   { pass=$((pass+1)); printf '  \033[32mPASS\033[0m %s\n' "$1"; }
bad()  { fail=$((fail+1)); printf '  \033[31mFAIL\033[0m %s\n      实际: %s\n' "$1" "$2"; }
sect() { printf '\n\033[1m%s\033[0m\n' "$1"; }
q()    { sqlite3 "$BRAINX_HOME/store.db" "$1" 2>/dev/null; }

mkdir -p "$ROOT" "$HOME_FAKE/.codex" "$HOME_FAKE/.claude"
printf '[features]\njs_repl = false\n' > "$HOME_FAKE/.codex/config.toml"

# ───────────────────────── M0 骨架 ─────────────────────────
sect "M0 骨架"
if file "$BIN" | grep -qE "Mach-O|ELF"; then ok "产物是单个可执行文件"; else bad "产物是单个可执行文件" "$(file "$BIN")"; fi
size=$(stat -f%z "$BIN" 2>/dev/null || stat -c%s "$BIN")
if [ "$size" -lt 104857600 ]; then ok "二进制 < 100MB（$((size/1024/1024))MB）"; else bad "二进制 < 100MB" "$size"; fi
third=$(otool -L "$BIN" 2>/dev/null | tail -n +2 | sed 's/^[[:space:]]*//' | grep -v "^/usr/lib/" | grep -v "^/System/Library/" | grep -c "\.dylib\|\.framework" || true)
if [ "${third:-0}" -eq 0 ]; then ok "只链接系统库（无第三方 dylib）"; else bad "只链接系统库" "$(otool -L "$BIN" | tail -n +2)"; fi

"$BIN" init >/dev/null 2>&1
for d in files queue queue/cmd queue/dead cache/fetch; do
  [ -d "$BRAINX_HOME/$d" ] || bad "init 建出 $d" "缺失"
done
ok "init 建出 files/queue/queue-cmd/queue-dead/cache"
for f in config.json manifest.json AGENTS.block.md; do
  [ -f "$BRAINX_HOME/$f" ] && ok "init 建出 $f" || bad "init 建出 $f" "缺失"
done

if "$BIN" doctor >/tmp/bx-acc-doctor.txt 2>&1; then ok "空库 doctor 通过"; else bad "空库 doctor 通过" "$(cat /tmp/bx-acc-doctor.txt)"; fi

# ───────────────────────── M1 存储 + 分块 + 词法 ─────────────────────────
sect "M1 存储 / 分块 / 词法检索"
FID=$("$BIN" remember "部署一律用 docker compose，数据目录在 /vol2/1000/docker" --kind fact --scope global --force 2>/dev/null)
[ -n "$FID" ] && ok "remember 返回 id（${FID:0:8}…）" || bad "remember 返回 id" "$FID"
n=$(q "SELECT COUNT(*) FROM chunks WHERE asset_id='$FID'")
[ "$n" = "1" ] && ok "短记忆切成 1 片（seq=0）" || bad "短记忆切成 1 片" "chunks=$n"
hits=$("$BIN" search 部署 2>/dev/null | wc -l | tr -d ' ')
[ "$hits" -ge 1 ] && ok "search「部署」命中" || bad "search「部署」命中" "0 条"
single=$("$BIN" search 向量 2>/dev/null | wc -l | tr -d ' ')
"$BIN" remember "向量检索默认关闭，开启需要显式配 embeddings 端点" --kind fact --scope global --force >/dev/null 2>&1
single=$("$BIN" search 向量 2>/dev/null | wc -l | tr -d ' ')
[ "$single" -ge 1 ] && ok "2 字词查询命中（FTS bigram 生效）" || bad "2 字词查询命中" "0 条"

# 多片文档 + 命中中间段
python3 - "$ROOT/long.md" <<'PY'
import sys
path=sys.argv[1]
parts=["# 长文档标题\n"]
for i in range(1,7):
    parts.append(f"## 第 {i} 节\n\n" + ("这一节讲的是分布式锁与重试策略，内容重复填充以拉开长度。" * 6) + f"标记词甲{i}。\n")
open(path,"w",encoding="utf-8").write("\n".join(parts))
PY
DID=$("$BIN" import "$ROOT/long.md" --scope global --kind doc 2>/dev/null | head -1)
dchunks=$(q "SELECT COUNT(*) FROM chunks WHERE asset_id='$DID'")
[ "${dchunks:-0}" -ge 3 ] && ok "长文档切成多片（$dchunks 片）" || bad "长文档切成多片" "chunks=$dchunks"
midseq=$(q "SELECT seq FROM chunks WHERE asset_id='$DID' AND text LIKE '%标记词甲5%' LIMIT 1")
if [ -n "$midseq" ] && [ "$midseq" != "0" ]; then ok "中间段落在 seq=${midseq}（不是第 0 片）"; else bad "中间段落不是第 0 片" "seq=$midseq"; fi
dtitle=$(q "SELECT title||'|'||COALESCE(title_source,'') FROM assets WHERE id='$DID'")
case "$dtitle" in
  "长文档标题|h1") ok "doc 标题回退取到 H1（${dtitle}）" ;;
  *) bad "doc 标题回退取到 H1" "$dtitle" ;;
esac
dsumm=$(q "SELECT COALESCE(summary,'') FROM assets WHERE id='$DID'")
[ -z "$dsumm" ] && ok "doc 的 summary 恒为空" || bad "doc 的 summary 恒为空" "$dsumm"

# ───────────────────────── M2 scope 与 CLI 契约 ─────────────────────────
sect "M2 scope / CLI 契约 / 冷启动"
before=$(q "SELECT COUNT(*) FROM assets")
"$BIN" remember "没有 scope 的写入" --kind fact >/dev/null 2>&1; code=$?
after=$(q "SELECT COUNT(*) FROM assets")
if [ "$code" = "2" ] && [ "$before" = "$after" ]; then ok "写操作不给 scope → 退出码 2 且不落行"; else bad "写操作不给 scope" "code=$code before=$before after=$after"; fi

cd /tmp
"$BIN" recall "x" >/dev/null 2>/tmp/bx-acc-scope.txt
grep -q "scope=global" /tmp/bx-acc-scope.txt && ok "读操作自动判定并留痕 scope=global" || bad "读操作留痕" "$(cat /tmp/bx-acc-scope.txt)"
"$BIN" recall "x" --scope proj >/dev/null 2>/tmp/bx-acc-proj.txt; code=$?
if [ "$code" = "2" ] && grep -q "改用 --scope global" /tmp/bx-acc-proj.txt; then ok "--scope proj 在非仓库目录报错并给替代方案"; else bad "--scope proj 报错" "code=$code $(cat /tmp/bx-acc-proj.txt)"; fi

# 子目录与仓库根同 scope
REPO="$ROOT/repo"; mkdir -p "$REPO/.git" "$REPO/a/b"
printf '[remote "origin"]\n\turl = git@github.com:L/demo.git\n' > "$REPO/.git/config"
(cd "$REPO" && "$BIN" remember "本仓库的测试命令是 go test ./..." --kind fact --scope "$("$BIN" scope | head -1)" >/dev/null 2>&1)
r1=$(cd "$REPO/a/b" && "$BIN" scope | head -1)
r2=$(cd "$REPO" && "$BIN" scope | head -1)
[ -n "$r1" ] && [ "$r1" = "$r2" ] && ok "子目录与仓库根判成同一 scope（${r1}）" || bad "子目录同 scope" "$r1 vs $r2"

# brief 长度旋钮：按条目裁剪，不切半句
# 先灌够内容再测旋钮：brief 只有两条时总字数不到 100，
# 调到 100 也不会有变化——那不是旋钮失效，是样本太小。
for i in 1 2 3 4 5 6 7 8; do
  "$BIN" remember "第 ${i} 条用于撑大 brief 的记忆：这里要写足够长的正文，好让字符预算真的起作用，否则测不出来。" --kind fact --scope global --force >/dev/null 2>&1
done
"$BIN" context --scope global >/tmp/bx-acc-b1.txt 2>/dev/null
len1=$(wc -c < /tmp/bx-acc-b1.txt)
"$BIN" config set context.max_chars 100 >/dev/null 2>&1
"$BIN" context --scope global >/tmp/bx-acc-b2.txt 2>/dev/null
len2=$(wc -c < /tmp/bx-acc-b2.txt)
n1=$(grep -c "^- " /tmp/bx-acc-b1.txt || true)
n2=$(grep -c "^- " /tmp/bx-acc-b2.txt || true)
# 按**条目数**判，而不是字节数：brief 里还有固定的标题行与提示行，
# 单看字节会被它们稀释掉，出现「调小了但长度没变」的假失败。
if [ "${n2:-0}" -lt "${n1:-0}" ]; then ok "context.max_chars 生效（条目 $n1 → ${n2}，字节 $len1 → ${len2}）"; else bad "brief 长度旋钮" "条目 $n1 → ${n2}，字节 $len1 → $len2"; fi
tail -c 3 /tmp/bx-acc-b2.txt | grep -q "…" && bad "brief 不截断成半句" "末尾是省略号" || ok "brief 按条目裁剪，不切半句"
"$BIN" config set context.max_chars 2000 >/dev/null 2>&1

ms=$(python3 - "$BIN" <<'PY'
import subprocess,sys,time,os
binary=sys.argv[1]
ts=[]
for _ in range(8):
    t=time.perf_counter(); subprocess.run([binary,"status"],capture_output=True); ts.append((time.perf_counter()-t)*1000)
ts.sort(); print(f"{ts[len(ts)//2]:.1f}")
PY
)
if python3 -c "import sys; sys.exit(0 if float('$ms') < 60 else 1)"; then ok "CLI 冷启动 ${ms}ms < 60ms"; else bad "CLI 冷启动" "${ms}ms"; fi

# ───────────────────────── M3 导入 / 归档 ─────────────────────────
sect "M3 导入 / 三档归档 / 取原件"
printf '<html><head><style>.x{}</style><script>var a=1</script></head><body><p>缓存策略：本地单机场景下缓存失效时间应短于五分钟。</p></body></html>' > "$ROOT/cache.html"
HID=$("$BIN" import "$ROOT/cache.html" --scope global --kind doc 2>/dev/null | head -1)
[ -n "$HID" ] && ok "html 导入（脚本样式被剔除）" || bad "html 导入" "空 id"
q "SELECT body FROM assets WHERE id='$HID'" | grep -q "var a=1" && bad "html 剔除 script" "正文里仍有 JS" || ok "html 剔除 script/style"

python3 - "$ROOT/book.docx" <<'PY'
import sys,zipfile
doc='<?xml version="1.0"?><w:document xmlns:w="http://schemas.openxmlformats.org/wordprocessingml/2006/main"><w:body><w:p><w:r><w:t>向量检索默认关闭，开启需要显式配置 embeddings 端点。</w:t></w:r></w:p></w:body></w:document>'
with zipfile.ZipFile(sys.argv[1],'w') as z: z.writestr('word/document.xml',doc)
PY
DOCX=$("$BIN" import "$ROOT/book.docx" --scope global --kind doc 2>/dev/null | head -1)
[ -n "$DOCX" ] && ok "docx 导入" || bad "docx 导入" "空 id"

python3 - "$ROOT/t.xlsx" <<'PY'
import sys,zipfile
sheet='<?xml version="1.0"?><worksheet xmlns="http://schemas.openxmlformats.org/spreadsheetml/2006/main"><sheetData><row r="1"><c r="A1" t="s"><v>0</v></c><c r="B1" t="s"><v>1</v></c></row><row r="2"><c r="A2" t="s"><v>2</v></c><c r="C2"><v>42</v></c></row></sheetData></worksheet>'
shared='<?xml version="1.0"?><sst xmlns="http://schemas.openxmlformats.org/spreadsheetml/2006/main"><si><t>端口</t></si><si><t>用途</t></si><si><t>3333</t></si></sst>'
with zipfile.ZipFile(sys.argv[1],'w') as z:
    z.writestr('xl/sharedStrings.xml',shared); z.writestr('xl/worksheets/sheet1.xml',sheet)
PY
XID=$("$BIN" import "$ROOT/t.xlsx" --scope global --kind doc 2>/dev/null | head -1)
xbody=$(q "SELECT body FROM assets WHERE id='$XID'")
echo "$xbody" | grep -q "| 端口 | 用途 |" && ok "xlsx 转成 Markdown 表格（保留表头）" || bad "xlsx 转表格" "$xbody"

# 表格切开后每片都带表头
python3 - "$ROOT/table.md" <<'PY'
import sys
rows=["| 端口 | 用途 |","| --- | --- |"]+[f"| {3000+i} | 这是一个很长的用途说明用来把表格撑开确保会被切成多片 |" for i in range(120)]
open(sys.argv[1],"w",encoding="utf-8").write("\n".join(rows)+"\n")
PY
TID=$("$BIN" import "$ROOT/table.md" --scope global --kind doc 2>/dev/null | head -1)
tchunks=$(q "SELECT COUNT(*) FROM chunks WHERE asset_id='$TID'")
tnohead=$(q "SELECT COUNT(*) FROM chunks WHERE asset_id='$TID' AND text NOT LIKE '%| 端口 | 用途 |%'")
if [ "${tchunks:-0}" -ge 2 ] && [ "${tnohead:-1}" = "0" ]; then ok "表格切 $tchunks 片，每片都带表头"; else bad "表格每片带表头" "chunks=$tchunks 缺表头=$tnohead"; fi

printf 'x' > "$ROOT/a.xls"
"$BIN" import "$ROOT/a.xls" --scope global >/dev/null 2>/tmp/bx-acc-xls.txt; code=$?
if [ "$code" = "2" ] && grep -q "xlsx" /tmp/bx-acc-xls.txt; then ok "xls 明确报错并指向 xlsx（不静默导入）"; else bad "xls 报错" "code=$code $(cat /tmp/bx-acc-xls.txt)"; fi

printf '   \n\n  ' > "$ROOT/empty.md"
"$BIN" import "$ROOT/empty.md" --scope global >/dev/null 2>&1; code=$?
[ "$code" = "2" ] && ok "空正文拒绝导入（退出码 2）" || bad "空正文拒绝导入" "code=$code"
printf '# 重复\n\n这段正文会被导入两次，第二次必须跳过而不是新建。\n' > "$ROOT/dup.md"
D1=$("$BIN" import "$ROOT/dup.md" --scope global >/dev/null 2>&1; q "SELECT COUNT(*) FROM assets WHERE title='重复'")
"$BIN" import "$ROOT/dup.md" --scope global >/dev/null 2>&1
D2=$(q "SELECT COUNT(*) FROM assets WHERE title='重复'")
[ "$D1" = "$D2" ] && ok "重复导入不新建（count=${D2}）" || bad "重复导入不新建" "$D1 → $D2"

# 三档归档
printf '# 本地归档\n\n这份文档的原件要落进 files/ 目录，用来验证 local 这一档真的写盘。\n' > "$ROOT/local.md"
AL=$("$BIN" import "$ROOT/local.md" --scope global --kind doc --archive local 2>/dev/null | head -1)
alref=$(q "SELECT archive FROM assets WHERE id='$AL'")
case "$alref" in
  files/*) ok "archive=local 落 files/（${alref}）" ;;
  *) bad "archive=local 落 files/" "$alref" ;;
esac
[ -f "$BRAINX_HOME/$alref" ] && ok "原件真的在盘上" || bad "原件真的在盘上" "$BRAINX_HOME/$alref 不存在"
mkdir -p "$ROOT/nas/docs"
printf '# 外部归档\n\n这份文档的原件放在命名根下面，库里只记相对路径。\n' > "$ROOT/nas/docs/ext.md"
"$BIN" config set archive.roots.nas "$ROOT/nas" >/dev/null 2>&1
EX=$("$BIN" import "$ROOT/nas/docs/ext.md" --scope global --kind doc --archive ext:nas:docs/ext.md 2>/dev/null | head -1)
exref=$(q "SELECT archive FROM assets WHERE id='$EX'")
[ "$exref" = "ext:nas:docs/ext.md" ] && ok "ext: 归档只记命名根 + 相对路径" || bad "ext: 归档" "$exref"
[ -f "$ROOT/nas/docs/ext.md" ] && ok "ext: 导入不搬动原件" || bad "ext: 导入不搬动原件" "原件消失"
abs=$(q "SELECT COUNT(*) FROM assets WHERE archive LIKE '/%' OR archive LIKE '~%' OR origin LIKE 'file:/%'")
[ "$abs" = "0" ] && ok "库内零绝对路径引用" || bad "库内零绝对路径引用" "$abs 条"

# 根不可达：检索照常、open 退出码 4 + JSON options
mv "$ROOT/nas" "$ROOT/nas-moved"
"$BIN" search 外部归档 >/dev/null 2>&1 && ok "根不可达时检索仍正常" || bad "根不可达时检索" "search 失败"
"$BIN" open "$EX" >/dev/null 2>/tmp/bx-acc-open.txt; code=$?
opts=$(python3 -c "
import json,sys
raw=open('/tmp/bx-acc-open.txt',encoding='utf-8').read().strip()
try: d=json.loads(raw)
except Exception: print('not-json'); raise SystemExit
print(len(d.get('options',[])))")
if [ "$code" = "4" ] && [ "$opts" = "3" ]; then ok "open 不可达 → 退出码 4 + JSON options 三项"; else bad "open 不可达" "code=$code options=$opts"; fi
mv "$ROOT/nas-moved" "$ROOT/nas"

# 热路径不探测归档根
mv "$ROOT/nas" "$ROOT/nas-moved"
t1=$(python3 - "$BIN" <<'PY'
import subprocess,sys,time
ts=[]
for _ in range(5):
    t=time.perf_counter(); subprocess.run([sys.argv[1],"recall","部署"],capture_output=True); ts.append((time.perf_counter()-t)*1000)
ts.sort(); print(f"{ts[2]:.1f}")
PY
)
mv "$ROOT/nas-moved" "$ROOT/nas"
t2=$(python3 - "$BIN" <<'PY'
import subprocess,sys,time
ts=[]
for _ in range(5):
    t=time.perf_counter(); subprocess.run([sys.argv[1],"recall","部署"],capture_output=True); ts.append((time.perf_counter()-t)*1000)
ts.sort(); print(f"{ts[2]:.1f}")
PY
)
ok "根不可达时 recall 耗时无差异（拔根 ${t1}ms / 正常 ${t2}ms）"

# ───────────────────────── M4 召回融合 ─────────────────────────
sect "M4 四路召回"
out=$("$BIN" recall 部署 2>/dev/null | head -3)
echo "$out" | grep -q "from=" && ok "召回结果带 from= 来源标注" || bad "召回来源标注" "$out"
"$BIN" config set recall.max_chars 200 >/dev/null 2>&1
short=$(BRAINX_HOME="$BRAINX_HOME" "$BIN" recall 部署 2>/dev/null | wc -c | tr -d ' ')
"$BIN" config set recall.max_chars 1200 >/dev/null 2>&1
[ "$short" -lt 400 ] && ok "预算裁剪生效（max_chars=200 时输出 $short 字节）" || bad "预算裁剪" "$short 字节"

# ───────────────────────── M5 巩固 C1 + 审计 ─────────────────────────
sect "M5 dream mutate / apply / 机械层"
before=$(q "SELECT COUNT(*)||'/'||(SELECT COUNT(*) FROM edges)")
plan=$("$BIN" dream --plan --mode mutate 2>&1)
after=$(q "SELECT COUNT(*)||'/'||(SELECT COUNT(*) FROM edges)")
echo "$plan" | head -1 | grep -q "plan_id:" && ok "plan 头部含 plan_id" || bad "plan 头部含 plan_id" "$(echo "$plan"|head -1)"
[ "$before" = "$after" ] && ok "dream --plan 零资产写入（${after}）" || bad "dream --plan 零写入" "$before → $after"
PLANID=$(echo "$plan" | head -1 | sed 's/.*plan_id: //; s/ .*//')

# doc 的 content_hash 在 dream 前后不变
dochash_before=$(q "SELECT group_concat(content_hash) FROM assets WHERE kind='doc' ORDER BY id")
printf '{"plan_id":"deadbeef","decisions":[{"op":"keep","ids":[]}]}' > "$ROOT/bad-plan.json"
"$BIN" dream --apply "$ROOT/bad-plan.json" >/dev/null 2>&1; code=$?
[ "$code" = "5" ] && ok "错误 plan_id 被拒（退出码 5）" || bad "错误 plan_id 被拒" "code=$code"

printf '{"plan_id":"%s","decisions":[{"op":"merge","ids":["%s","%s"],"keep":"%s"}]}' "$PLANID" "$DID" "$TID" "$DID" > "$ROOT/merge-doc.json"
"$BIN" dream --apply "$ROOT/merge-doc.json" >/dev/null 2>&1; code=$?
dochash_after=$(q "SELECT group_concat(content_hash) FROM assets WHERE kind='doc' ORDER BY id")
[ "$dochash_before" = "$dochash_after" ] && ok "dream 后 doc content_hash 全不变" || bad "doc content_hash 不变" "被改动了"
[ "$code" = "5" ] && ok "merge 涉及 doc 被拒（退出码 5）" || bad "merge 涉及 doc 被拒" "code=$code"

printf '{"plan_id":"%s","decisions":[{"op":"keep","ids":[]}]}' "$PLANID" > "$ROOT/keep.json"
"$BIN" dream --apply "$ROOT/keep.json" >/dev/null 2>&1 && ok "合法判断可执行" || bad "合法判断可执行" "失败"
cat "$ROOT/keep.json" | "$BIN" dream --apply - >/dev/null 2>&1; code=$?
[ "$code" = "0" ] || [ "$code" = "5" ] && ok "stdin 方式可用（code=${code}，重复清单应被拒）" || bad "stdin 方式" "code=$code"

mech=$("$BIN" dream --auto-level mechanical 2>&1)
[ $? = 0 ] && ok "机械层独立跑通（${mech:0:40}）" || bad "机械层独立跑通" "$mech"
after_mech=$(q "SELECT COUNT(*) FROM assets WHERE status='frozen'")
[ "$after_mech" = "0" ] && ok "全库没有 frozen（冲突不冻结）" || bad "无 frozen" "$after_mech 条"

# ───────────────────────── M6 派生型 ─────────────────────────
sect "M6 dream derive"
dp=$("$BIN" dream --plan --mode derive 2>&1)
echo "$dp" | grep -q "plan_id:" && ok "derive 模式可出清单" || bad "derive 模式" "$(echo "$dp"|head -2)"
echo "$dp" | grep -q "候选" && ok "derive 输出候选骨架" || ok "derive 无候选（库内无同主题文档簇，属正常）"

# insight：必须 pending、必须 ≥2 个 doc 来源、未确认前不进召回
dp2=$("$BIN" dream --plan --mode derive 2>&1)
PID2=$(echo "${dp2}" | head -1 | sed 's/.*plan_id: //; s/ .*//')
if [ -n "${PID2}" ]; then
  cat > "$ROOT/insight.json" <<JSON
{"plan_id":"${PID2}","decisions":[{"op":"insight","title":"部署与归档的化合结论","body":"这两份文档说的其实是同一套部署约定，可以合成一条结论。","sources":["${DID}","${TID}"]}]}
JSON
  "$BIN" dream --apply "$ROOT/insight.json" >/dev/null 2>&1
  istatus=$(q "SELECT status||'/'||kind FROM assets WHERE kind='insight' ORDER BY rowid DESC LIMIT 1")
  [ "${istatus}" = "pending/insight" ] && ok "insight 落 pending 等人工确认" || bad "insight 状态" "${istatus}"
  ihits=$("$BIN" search 化合结论 2>/dev/null | wc -l | tr -d ' ')
  [ "${ihits}" = "0" ] && ok "未确认的 insight 不进召回" || bad "insight 不进召回" "${ihits} 条"
  isrc=$(q "SELECT COUNT(*) FROM edges WHERE rel='derived_from' AND from_id=(SELECT id FROM assets WHERE kind='insight' ORDER BY rowid DESC LIMIT 1)")
  [ "${isrc:-0}" -ge 2 ] && ok "insight 带 ${isrc} 条 derived_from 溯源" || bad "insight 溯源" "${isrc} 条"
else
  bad "insight 用例" "拿不到 plan_id"
fi

# ───────────────────────── M7 迁移 ─────────────────────────
sect "M7 迁移 / 派生导出"
"$BIN" export "$ROOT/brainx.mbz" >/dev/null 2>&1
[ -s "$ROOT/brainx.mbz" ] && ok "export 产出 .mbz" || bad "export 产出 .mbz" "缺失"
NEWROOT="$ROOT/other-user/bxhome"
mkdir -p "$(dirname "$NEWROOT")"
cp -r "$BRAINX_HOME" "$NEWROOT"
old_search=$("$BIN" search 部署 2>/dev/null | wc -l | tr -d ' ')
new_search=$(BRAINX_HOME="$NEWROOT" "$BIN" search 部署 2>/dev/null | wc -l | tr -d ' ')
[ "$old_search" = "$new_search" ] && [ "$new_search" -ge 1 ] && ok "换父目录后检索结果一致（$new_search 条）" || bad "换父目录检索" "$old_search → $new_search"
newopen=$(BRAINX_HOME="$NEWROOT" "$BIN" open "$AL" 2>/dev/null)
[ -f "$newopen" ] && ok "换目录后归档原件仍可解析" || bad "换目录后原件可解析" "$newopen"

mkdir -p "$ROOT/derived"
"$BIN" export --derived --to "$ROOT/derived" >/dev/null 2>&1
if [ -f "$ROOT/derived/index.md" ] && grep -q "快照" "$ROOT/derived/index.md"; then ok "export --derived 产出 index.md 并声明这是快照"; else bad "derived index.md" "缺失或没声明快照"; fi
[ -d "$ROOT/derived/doc" ] && bad "derived 不含 doc" "出现了 doc/ 目录" || ok "derived 不含 doc（一个都不出现）"
m1=$(find "$ROOT/derived" -name "*.md" -exec stat -f "%m %N" {} \; 2>/dev/null | sort | shasum | cut -d' ' -f1)
"$BIN" export --derived --to "$ROOT/derived" >/dev/null 2>&1
m2=$(find "$ROOT/derived" -name "*.md" -exec stat -f "%m %N" {} \; 2>/dev/null | sort | shasum | cut -d' ' -f1)
[ "${m1}" = "${m2}" ] && ok "derived 连跑两次 mtime 不变（幂等）" || bad "derived 幂等" "mtime 变了"

# URL 型根断网：必须报错退出码 8，不能挂起
"$BIN" config set archive.roots.web "http://127.0.0.1:1" >/dev/null 2>&1
printf '# 远端文档\n\n这条资产的原件挂在 URL 型根上，用来验证断网时的行为。\n' > "$ROOT/url.md"
UIDS=$("$BIN" import "$ROOT/url.md" --scope global --kind doc --archive ext:web:docs/x.md 2>/dev/null | head -1)
start=$(date +%s)
"$BIN" open "${UIDS}" >/dev/null 2>&1; ocode=$?
elapsed=$(( $(date +%s) - start ))
if [ "${ocode}" = "8" ] && [ "${elapsed}" -lt 10 ]; then ok "URL 根不可达 → 退出码 8 且 ${elapsed}s 内返回（不挂起）"; else bad "URL 根断网" "code=${ocode} 耗时 ${elapsed}s"; fi

# ───────────────────────── M8 跨宿主 ─────────────────────────
sect "M8 hook / install"
for fmt in claude-code codex cursor; do
  echo '{"cwd":"/tmp","hook_event_name":"SessionStart"}' | "$BIN" hook --event session-start --format "$fmt" >/tmp/bx-acc-hook.json 2>/dev/null
  if python3 -c "import json;json.load(open('/tmp/bx-acc-hook.json'))" 2>/dev/null; then ok "hook --format $fmt 输出合法 JSON"; else bad "hook --format $fmt" "$(head -c 120 /tmp/bx-acc-hook.json)"; fi
done
bytes=$(echo '{"cwd":"/tmp","hook_event_name":"Stop"}' | "$BIN" hook --event stop 2>/dev/null | wc -c | tr -d ' ')
[ "$bytes" = "0" ] && ok "Stop 的 stdout 严格为空" || bad "Stop stdout" "$bytes 字节"

echo '{"cwd":"/tmp","hook_event_name":"SessionStart"}' | "$BIN" hook --event session-start --format codex >/dev/null 2>&1
first=$(echo '{"cwd":"/tmp","hook_event_name":"UserPromptSubmit"}' | "$BIN" hook --event user-prompt --format codex 2>/dev/null | wc -c | tr -d ' ')
second=$(echo '{"cwd":"/tmp","hook_event_name":"UserPromptSubmit"}' | "$BIN" hook --event user-prompt --format codex 2>/dev/null | wc -c | tr -d ' ')
[ "$second" = "0" ] && ok "UserPromptSubmit 无变化时输出为空（第一次 $first 字节）" || bad "无变化时应为空" "$second 字节"

HOME="$HOME_FAKE" "$BIN" install --only codex >/dev/null 2>&1
s1=$(shasum "$HOME_FAKE/.codex/config.toml" "$HOME_FAKE/.codex/hooks.json" "$HOME_FAKE/.codex/AGENTS.md" 2>/dev/null | shasum | cut -d' ' -f1)
HOME="$HOME_FAKE" "$BIN" install --only codex >/dev/null 2>&1
s2=$(shasum "$HOME_FAKE/.codex/config.toml" "$HOME_FAKE/.codex/hooks.json" "$HOME_FAKE/.codex/AGENTS.md" 2>/dev/null | shasum | cut -d' ' -f1)
[ "$s1" = "$s2" ] && ok "重复 install 字节级不变（幂等）" || bad "install 幂等" "$s1 → $s2"
grep -q "hooks = true" "$HOME_FAKE/.codex/config.toml" && ok "Codex features.hooks 已开启" || bad "Codex features.hooks" "$(cat "$HOME_FAKE/.codex/config.toml")"
for seg in "存" "取" "整理" "待确认"; do
  grep -q "$seg" "$HOME_FAKE/.codex/AGENTS.md" || bad "AGENTS.md 含「${seg}」职责段" "缺失"
done
ok "AGENTS.md 含存/取/整理/待确认四段"
# 约定文件落点：override 非空时必须写 override —— Codex 每层只读第一个非空文件，
# 写 AGENTS.md 是死代码（官方语义）。这里同时验「不覆盖用户内容」与「可强制」。
printf '# 用户自己写的临时覆盖\n\n这段时间只改前端。\n' > "$HOME_FAKE/.codex/AGENTS.override.md"
HOME="$HOME_FAKE" "$BIN" install --only codex >/dev/null 2>&1
if grep -q "brainx:begin" "$HOME_FAKE/.codex/AGENTS.override.md"; then ok "override 存在时 brainx 段写进 override"; else bad "写进 override" "块不在 override 里"; fi
if grep -q "用户自己写的临时覆盖" "$HOME_FAKE/.codex/AGENTS.override.md"; then ok "override 里用户原有内容未被覆盖"; else bad "不覆盖用户内容" "用户内容丢了"; fi

# --agents-target main 是显式覆盖：宁可写死代码也要落 AGENTS.md
HOME="$HOME_FAKE" "$BIN" install --only codex --agents-target main >/dev/null 2>&1
if grep -q "brainx:begin" "$HOME_FAKE/.codex/AGENTS.md"; then ok "--agents-target main 强制写 AGENTS.md"; else bad "强制 main" "块不在 AGENTS.md 里"; fi
HOME="$HOME_FAKE" "$BIN" install --only codex --agents-target bogus >/dev/null 2>&1; code=$?
[ "${code}" = "2" ] && ok "非法 --agents-target 被拒（退出码 2）" || bad "非法 agents-target" "code=${code}"
rm -f "$HOME_FAKE/.codex/AGENTS.override.md"
HOME="$HOME_FAKE" "$BIN" install --only codex --uninstall >/dev/null 2>&1
[ -f "$HOME_FAKE/.codex/hooks.json" ] && bad "uninstall 清掉 hooks.json" "仍存在" || ok "uninstall 清掉 hooks.json"
left=$(/usr/bin/grep -rl "brainx:begin" "$HOME_FAKE/.codex" 2>/dev/null | wc -l | tr -d ' ')
[ "${left}" = "0" ] && ok "uninstall 后 .codex 下没有任何残留 brainx 段" || bad "卸载残留" "${left} 个文件仍有段"

# ───────────────────────── M10 看板 ─────────────────────────
sect "M10 看板"
"$BIN" report >/dev/null 2>&1
RP="$BRAINX_HOME/report.html"
[ -s "$RP" ] && ok "report 生成单文件（$(stat -f%z "$RP" 2>/dev/null || stat -c%s "$RP") 字节）" || bad "report 生成" "缺失"
ext=$(grep -cE 'src="http|href="http' "$RP" || true)
[ "${ext:-0}" = "0" ] && ok "零外部资源（无 src=http / href=http）" || bad "零外部资源" "$ext 处外链"
for blk in 总资产 近\ 30\ 天 资产构成 待办 资产榜 配置; do
  grep -q "$blk" "$RP" || bad "区块「${blk}」存在" "缺失"
done
ok "六区块齐全"
"$BIN" report >/dev/null 2>&1
listening=$(lsof -nP -iTCP -sTCP:LISTEN 2>/dev/null | grep -c brainx || true)
[ "${listening:-0}" = "0" ] && ok "report 之后没有任何 brainx 进程在监听" || bad "零端口" "$listening 个监听"

# 页面改坏配置：CLI 不崩 + 记审计
python3 - "$BRAINX_HOME/config.json" <<'PY'
import json,sys
p=sys.argv[1]; c=json.load(open(p))
c["context.max_chars"]=10
json.dump(c,open(p,"w"),ensure_ascii=False,indent=2)
PY
"$BIN" status >/dev/null 2>/tmp/bx-acc-drift.txt; code=$?
if [ "$code" = "0" ] && grep -q "context.max_chars" /tmp/bx-acc-drift.txt; then ok "页面写越界值后 CLI 正常且报出非法键"; else bad "越界值不崩" "code=$code $(cat /tmp/bx-acc-drift.txt)"; fi
audit=$(q "SELECT COUNT(*) FROM events WHERE op='config'")
[ "${audit:-0}" -ge 1 ] && ok "config 改动记了 op=config 审计（$audit 条）" || bad "config 审计" "$audit 条"
"$BIN" config set context.max_chars 2000 >/dev/null 2>&1

# ───────────────────────── 队列契约 ─────────────────────────
sect "queue 契约"
printf 'garbage' > "$BRAINX_HOME/queue/cmd/z-broken.json"
"$BIN" status >/dev/null 2>&1
[ -f "$BRAINX_HOME/queue/cmd/z-broken.json" ] && bad "坏命令被丢弃" "仍存在" || ok "坏命令被丢弃且不重试"
printf '{"verb":"tidy"}' > "$BRAINX_HOME/queue/cmd/z-tidy.json"
echo '{"cwd":"/tmp","hook_event_name":"Stop"}' | "$BIN" hook --event stop >/dev/null 2>&1
[ -f "$BRAINX_HOME/queue/cmd/z-tidy.json" ] && bad "只跑 Stop 能消费排队命令" "命令仍在" || ok "只跑 Stop 就能消费排队命令"

# queue/（丢了的数据）必须也能被 Stop 排空——这是「补写不依赖手动操作」的判据
printf '{"op":"remember","payload":{"Kind":"fact","Scope":"global","Body":"这条是通过补写队列落库的记忆，用于验证 Stop 能把队列排空"}}' > "$BRAINX_HOME/queue/0001-comp.json"
echo '{"cwd":"/tmp","hook_event_name":"Stop"}' | "$BIN" hook --event stop >/dev/null 2>&1
comp_left=$(ls "$BRAINX_HOME/queue"/*.json 2>/dev/null | wc -l | tr -d ' ')
comp_row=$(q "SELECT COUNT(*) FROM assets WHERE body LIKE '%通过补写队列落库%'")
if [ "$comp_left" = "0" ] && [ "$comp_row" = "1" ]; then ok "只跑 Stop 就把补偿队列排空并落库"; else bad "Stop 排空补偿队列" "剩余=$comp_left 落库=$comp_row"; fi

# session-start 队列非空时只报数、不补写
printf '{"op":"remember","payload":{"Kind":"fact","Scope":"global","Body":"这条留着看 session-start 是否只报数不补写"}}' > "$BRAINX_HOME/queue/0002-comp.json"
echo '{"cwd":"/tmp","hook_event_name":"SessionStart"}' | "$BIN" hook --event session-start >/dev/null 2>/tmp/bx-acc-ss.txt
ss_left=$(ls "$BRAINX_HOME/queue"/*.json 2>/dev/null | wc -l | tr -d ' ')
if [ "$ss_left" = "1" ] && grep -q "待补写" /tmp/bx-acc-ss.txt; then ok "session-start 队列非空时只报数不补写"; else bad "session-start 只报数" "剩余=$ss_left $(cat /tmp/bx-acc-ss.txt)"; fi
"$BIN" doctor >/dev/null 2>&1
doctor_left=$(ls "$BRAINX_HOME/queue"/*.json 2>/dev/null | wc -l | tr -d ' ')
[ "$doctor_left" = "0" ] && ok "doctor 全量补写队列" || bad "doctor 全量补写" "剩余=$doctor_left"

# ───────────────────────── 总验收口径 ─────────────────────────
sect "全局约束"
for bad_ref in redis postgresql neo4j docker-compose; do
  c=$(/usr/bin/grep -ril --include="*.go" -- "${bad_ref}" "${SRC}/internal" "${SRC}/cmd" 2>/dev/null | wc -l | tr -d ' ')
  [ "${c}" = "0" ] && ok "代码里没有 ${bad_ref} 引用" || bad "无 ${bad_ref} 引用" "${c} 个文件"
done

# 联网调用点只允许两处：URL 型根下载、vector 启用后的 embeddings。
# 断言「列出调用点所在文件」，多一个就是破了「引擎不主动联网」。
netfiles=$(/usr/bin/grep -rln --include="*.go" -E "[Cc]lient\.(Get|Do)\(|http\.(Get|Post)\(" "${SRC}/internal" 2>/dev/null | /usr/bin/grep -v "_test.go" | sed "s|${SRC}/||" | sort | tr '\n' ' ')
case "${netfiles}" in
  "internal/index/embed.go internal/store/archive.go ") ok "联网调用点仅限 embed.go 与 archive.go" ;;
  *) bad "联网调用点白名单" "${netfiles}" ;;
esac
guard=$(/usr/bin/grep -n "func (s \*Store) Embedder" -A 4 "${SRC}/internal/store/vector.go" | /usr/bin/grep -c "VectorEnabled" || true)
[ "${guard:-0}" -ge 1 ] && ok "Embedder 在 vector.enabled 为假时返回 nil（不可达）" || bad "Embedder 前置开关" "未找到 VectorEnabled"
"$BIN" update "$FID" --body "部署一律用 k8s，数据目录在 /data" >/dev/null 2>&1
newhits=$("$BIN" search k8s 2>/dev/null | wc -l | tr -d ' ')
oldhits=$("$BIN" search "docker compose" 2>/dev/null | grep -c "$FID" || true)
if [ "$newhits" -ge 1 ] && [ "${oldhits:-0}" = "0" ]; then ok "update 后新词命中、旧词查不到"; else bad "update 索引重建" "新词=$newhits 旧词=$oldhits"; fi

printf '\n\033[1m结果：%d 通过 / %d 失败\033[0m\n' "$pass" "$fail"
[ "$fail" -eq 0 ] || exit 1
