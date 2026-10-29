package report

import (
	"encoding/json"
	"fmt"
	"html/template"
	"os"
	"path/filepath"
	"strings"

	"github.com/ksi512/BrainX-prototype/internal/store"
)

// 看板：一个命令生成单文件 HTML，file:// 直接打开。
// 零端口、零常驻、不引 CDN、断网可用、整块可删。
//
// 页面唯一能写的两个目标（硬编码白名单，不接受参数）：
//  1. config.json —— 配置区
//  2. queue/cmd/<id>-<verb>.json —— 命令队列（静态页起不了进程，只能排队）
//
// 除此之外不列目录、不读 store.db、不碰 files/、不删任何文件。

const page = `<!doctype html><html lang="zh-CN"><head><meta charset="utf-8">
<meta name="viewport" content="width=device-width,initial-scale=1">
<title>brainx 看板</title><style>
:root{--ink:#161d29;--muted:#68727f;--line:#e2e6eb;--paper:#fbfaf7;--panel:#fff;--blue:#246bce;--amber:#b45309;--red:#b42318;--green:#0f7b46}
*{box-sizing:border-box}body{margin:0;background:var(--paper);color:var(--ink);font:14px/1.55 -apple-system,'PingFang SC','Noto Sans SC',system-ui,sans-serif}
.wrap{max-width:1080px;margin:auto;padding:40px 22px 90px}
header{display:flex;justify-content:space-between;align-items:flex-end;gap:16px;border-bottom:2px solid var(--ink);padding-bottom:18px}
h1{font-size:26px;margin:0 0 6px}h2{font-size:15px;margin:0 0 4px;letter-spacing:.02em}
h3{font-size:13px;margin:0 0 8px;color:var(--muted);font-weight:600}
.meta,.muted{color:var(--muted);font:12px ui-monospace,Menlo,monospace}
.sub{color:var(--muted);font-size:12px;margin:0 0 10px}
.kpis{display:grid;grid-template-columns:repeat(4,1fr);gap:12px;margin-top:22px}
.card{background:var(--panel);border:1px solid var(--line);border-radius:10px;padding:15px 16px}
.label{color:var(--muted);font-size:12px;display:flex;align-items:center;gap:5px}
.num{font:700 27px/1.1 ui-monospace,Menlo,monospace;margin:7px 0 5px;font-variant-numeric:tabular-nums}
.num-sm{font:600 12px ui-monospace,Menlo,monospace;min-width:38px;text-align:right}
.info{display:inline-flex;align-items:center;justify-content:center;width:14px;height:14px;border-radius:50%;background:#e8eef8;color:#3b5f97;font:600 10px/1 ui-monospace;cursor:help}
.warn .num{color:var(--amber)}
section{margin-top:20px;background:var(--panel);border:1px solid var(--line);border-radius:10px;padding:18px}
.sec-head{display:flex;justify-content:space-between;align-items:baseline;gap:12px;flex-wrap:wrap}
.legend{display:flex;gap:14px;font-size:12px;color:var(--muted);flex-wrap:wrap}
.legend span{cursor:pointer;user-select:none;display:inline-flex;align-items:center;gap:5px}
.legend .sw{width:11px;height:11px;border-radius:2px;display:inline-block}
.legend .off{opacity:.38;text-decoration:line-through}
.bar-row{display:flex;align-items:center;gap:10px;margin:7px 0;font-size:12px}
.bar-row code{width:78px;color:var(--ink)}
.track{height:9px;flex:1;background:#eef0f3;border-radius:9px;overflow:hidden}
.fill{height:100%;background:var(--blue);border-radius:9px}
.cols{display:grid;grid-template-columns:1.15fr 1fr .85fr;gap:14px;margin-top:14px}
details{border:1px solid var(--line);border-radius:8px;padding:8px 10px;margin-bottom:7px;background:#fcfcfd}
details>summary{cursor:pointer;font-size:12px;display:flex;justify-content:space-between;gap:8px}
details[open]>summary{margin-bottom:7px;border-bottom:1px solid var(--line);padding-bottom:6px;display:flex;justify-content:space-between}
.item{display:flex;justify-content:space-between;align-items:center;gap:8px;padding:4px 0;font-size:12px}
.item .t{overflow:hidden;text-overflow:ellipsis;white-space:nowrap}
.tabs{display:flex;gap:6px;margin:12px 0 8px;flex-wrap:wrap}
.tab{border:1px solid var(--line);background:#fff;border-radius:20px;padding:5px 12px;font-size:12px;cursor:pointer;color:var(--muted)}
.tab.on{background:var(--ink);color:#fff;border-color:var(--ink)}
table{width:100%;border-collapse:collapse}
th,td{text-align:left;padding:7px 8px;border-bottom:1px solid var(--line);font-size:12px;vertical-align:top}
th{color:var(--muted);font-weight:600}
td.t{max-width:420px;overflow:hidden;text-overflow:ellipsis;white-space:nowrap}
code{font:12px ui-monospace,Menlo,monospace}
.btn{border:1px solid var(--line);background:#fff;border-radius:7px;padding:5px 10px;font-size:12px;cursor:pointer}
.btn:hover{border-color:#b9c2cd}.btn.strong{background:var(--ink);color:#fff;border-color:var(--ink)}
textarea{width:100%;min-height:78px;border:1px solid var(--line);border-radius:8px;padding:10px;font:12px/1.6 ui-monospace,Menlo,monospace;background:#fcfcfd;resize:vertical}
.grp{margin-top:10px}
.row{display:flex;align-items:center;gap:10px;padding:6px 0;border-bottom:1px dashed #eef0f3;font-size:12px}
.row .k{width:230px;font:12px ui-monospace,Menlo,monospace}
.row input[type=number],.row input[type=text]{width:120px;border:1px solid var(--line);border-radius:6px;padding:4px 7px;font:12px ui-monospace,Menlo,monospace}
.row select{border:1px solid var(--line);border-radius:6px;padding:4px 7px;font:12px -apple-system,'PingFang SC',system-ui,sans-serif;background:#fff}
.arch{display:flex;gap:6px;align-items:center;flex-wrap:wrap}
.arch input[type=text]{width:220px}
.unit{color:var(--muted);font-size:11px}
.row .d{flex:1;color:var(--muted)}
.ro{color:var(--muted);font-size:11px;border:1px solid var(--line);border-radius:5px;padding:2px 6px;background:#f5f6f7;min-width:120px;max-width:320px;display:inline-block;line-height:1.45}
.note{background:#eef5ff;border:1px solid #cfe1fb;border-radius:9px;padding:11px 13px;margin-top:16px;font-size:12px;color:#274265}
.msg{margin-top:10px;font-size:12px;min-height:18px;color:var(--green)}
.msg.err{color:var(--red)}
footer{margin-top:26px;color:var(--muted);font-size:11px;text-align:center;line-height:1.9}
.axis{font:10px ui-monospace,Menlo,monospace;fill:#8b95a1}
.chart{overflow-x:auto;-webkit-overflow-scrolling:touch;margin-top:10px}
.chart svg{display:block;width:100%;min-width:680px;height:auto}
.chart-hint{display:none;color:var(--muted);font-size:11px;margin:6px 0 0}
@media(max-width:720px){.chart-hint{display:block}}
@media(max-width:820px){.kpis{grid-template-columns:repeat(2,1fr)}.cols{grid-template-columns:1fr}.row{flex-wrap:wrap}.row .k{width:100%}header{display:block}td.t{max-width:160px}}
</style></head><body><main class="wrap">

<header><div><h1>brainx 看板</h1>
<div class="meta">$BRAINX_HOME = {{.Home}} · 生成于 {{.Generated}}</div></div>
<div class="meta">单文件静态 · 零端口 · 断网可用</div></header>

<div class="note">全部数字取自<b>同一次</b> SQLite 快照，所以彼此自洽；页面本身不修改任何资产。每个数字旁的 ⓘ 是它的口径——没有口径的数字等于错误的数字。</div>

<div class="kpis">
 <div class="card"><div class="label">总资产 <span class="info" title="assets 表全部行数，含已归档与被合并的">i</span></div>
  <div class="num">{{.Total}}</div><div class="muted">active {{.Active}} · archived {{.Archived}}</div></div>
 <div class="card"><div class="label">今日入库 <span class="info" title="本地时区今日 00:00 起创建的行数，含 pending">i</span></div>
  <div class="num">{{.Today}}</div><div class="muted">今天 00:00 起</div></div>
 <div class="card"><div class="label">近 7 日 <span class="info" title="含今天在内的 7 天；环比 = 与再往前 7 天相比的变化百分比">i</span></div>
  <div class="num">{{.Last7}}</div>
  <div class="muted">环比 {{.WeekDeltaText}} · 上一 7 日 {{.Prev7}}</div></div>
 <div class="card warn"><div class="label">待处理 <span class="info" title="pending 资产 + 未裁决冲突 + queue 待补写，三者相加">i</span></div>
  <div class="num">{{.TodoTotal}}</div>
  <div class="muted">pending {{.Pending}} · 冲突 {{.OpenReviews}} · queue {{.QueuePending}}</div></div>
</div>

<section><div class="sec-head"><div><h2>近 30 天 <span class="info" title="柱=每日入库（左轴）；线=每日召回（右轴），含 recall 查询与 context/hook 注入；虚线是各自均值">i</span></h2>
<p class="sub">入库高而召回低 = <b>存了但搜不到</b>，比没存更糟。30 天召回 {{.RecallCount30}} 次，其中零命中 {{.ZeroHitRecalls30}} 次。</p></div>
<div class="legend"><span data-series="series-imports"><i class="sw" style="background:#246bce"></i>每日入库（均值 {{.ImportMeanText}}）</span>
<span data-series="series-recalls"><i class="sw" style="background:#b45309"></i>每日召回（均值 {{.RecallMeanText}}）</span></div></div>
<div class="chart">{{.Chart}}</div>
<p class="chart-hint">← 横向滑动查看完整 30 天 →</p></section>

<section><h2>资产构成 <span class="info" title="只统计 status=active 的资产，按 kind 分组">i</span></h2>
<p class="sub">某类 kind 长期不出现，说明那类内容根本没进来；进来了却不在召回榜里，说明存了却搜不到。</p>
{{.KindBars}}</section>

<section><div class="sec-head"><div><h2>待办</h2><p class="sub">三栏各自指向一个动作，不是同一件事的三种说法。</p></div>
<div style="display:flex;gap:8px;flex-wrap:wrap">
<button class="btn" onclick="queueCmd('tidy')">排入机械层</button>
<button class="btn" onclick="queueCmd('drain')">排入补写重放</button>
</div></div>
<div class="cols">
 <div class="col"><h3>待确认 {{.Pending}} <span class="info" title="按 pending_reason 分组；确认/拒绝都会同步清理相关冲突">i</span></h3>
  {{.PendingHTML}}</div>
 <div class="col"><h3>未裁决冲突 {{.OpenReviews}} <span class="info" title="reviews 表 resolved_at 为空的行；两条都仍是 active">i</span></h3>
  {{.ReviewHTML}}</div>
 <div class="col"><h3>补偿队列 <span class="info" title="queue/ 是丢了的数据（需重试）；queue/cmd 是命令（执行一次即删）。两者故意分开">i</span></h3>
  <div class="item"><span class="t">待补写</span><b>{{.QueuePending}}</b></div>
  <div class="item"><span class="t">死信</span><b>{{.QueueDead}}</b></div>
  <div class="muted" style="margin-top:8px">死信移回：<code>brainx doctor --retry-dead</code></div></div>
</div></section>

<section><div class="sec-head"><div><h2>资产榜</h2><p class="sub">不用 use_count 排序——那是累计值，新旧资产不可比。</p></div></div>
<div class="tabs">
 <button class="tab on" data-tab="tab-used">近 30 天最常用</button>
 <button class="tab" data-tab="tab-decay">快被衰减杀掉</button>
 <button class="tab" data-tab="tab-never">从未被召回</button>
</div>
<div id="tab-used">{{.MostUsedHTML}}</div>
<div id="tab-decay" hidden><p class="sub">importance ≥ 50 且 ≥ 60 天没被召回，且处在会衰减的层（doc 永不衰减）。</p>{{.DecayHTML}}</div>
<div id="tab-never" hidden><p class="sub">入库超过 7 天、一次都没被召回。<b>标题来源</b>很关键：自动回落的标题往往是搜不到的根因。</p>{{.NeverHTML}}</div>
</section>

<section><div class="sec-head"><div><h2>配置</h2>
<p class="sub">页面只写 <code>config.json</code> 一个文件名，且只改你动过的键——其余原样保留。</p></div>
<div style="display:flex;gap:8px;flex-wrap:wrap"><button class="btn strong" onclick="writeConfig()">写入 config.json</button>
<button class="btn" onclick="pickDir()">选择 $BRAINX_HOME</button></div></div>
<div class="note" id="cap">尚未选择目录。浏览器需要一次目录授权（Chrome / Edge 支持；不支持就复制下方命令到终端执行）。最坏退化：每次打开页面重选一次，约 2 秒。</div>
<div id="groups"></div>
<h3 style="margin-top:18px">待执行的命令 <span class="info" title="改动的键会实时渲染成 CLI 命令；复制到终端执行等同于在这里点写入">i</span></h3>
<textarea id="cmds" readonly placeholder="（改了上面的旋钮后，这里会出现对应的 brainx config set 命令）"></textarea>
<div class="msg" id="msg"></div></section>

<footer>store.db {{.BytesText}} · 原件 {{.FileMB}} MB · chunks {{.Chunks}} · edges {{.Edges}}<br>
这一页是生成物：重新运行 <code>brainx report</code> 会覆盖它，它自己不会后台更新。</footer>
</main>
<script id="payload" type="application/json">{{.Payload}}</script>
<script>
var P = JSON.parse(document.getElementById('payload').textContent);
var dirHandle = null;
function msg(text, bad){var el=document.getElementById('msg');el.textContent=text;el.className=bad?'msg err':'msg';}
function cap(text){document.getElementById('cap').textContent=text;}

Array.prototype.forEach.call(document.querySelectorAll('.legend span'), function(span){
  span.addEventListener('click', function(){
    var group = document.querySelector('.' + span.getAttribute('data-series'));
    if(!group) return;
    var hidden = group.style.display === 'none';
    group.style.display = hidden ? '' : 'none';
    span.classList.toggle('off', !hidden);
  });
});

Array.prototype.forEach.call(document.querySelectorAll('.tab'), function(tab){
  tab.addEventListener('click', function(){
    Array.prototype.forEach.call(document.querySelectorAll('.tab'), function(t){t.classList.remove('on');});
    tab.classList.add('on');
    ['tab-used','tab-decay','tab-never'].forEach(function(id){
      document.getElementById(id).hidden = (id !== tab.getAttribute('data-tab'));
    });
  });
});

function specOf(name){
  for(var i=0;i<P.keys.length;i++){ if(P.keys[i].name===name) return P.keys[i]; }
  return null;
}

function fillGroups(){
  var host = document.getElementById('groups');
  var order = [], byGroup = {};
  P.keys.forEach(function(k){
    if(!byGroup[k.group]){byGroup[k.group]=[];order.push(k.group);}
    byGroup[k.group].push(k);
  });
  order.forEach(function(group){
    var box = document.createElement('details');
    box.className = 'grp';
    if(order.indexOf(group) === 0){ box.open = true; }
    var sum = document.createElement('summary');
    var left = document.createElement('span');
    left.textContent = byGroup[group][0].groupLabel || group;
    var right = document.createElement('span');
    right.className = 'muted';
    right.textContent = byGroup[group].length + ' 项';
    sum.appendChild(left); sum.appendChild(right);
    box.appendChild(sum);
    byGroup[group].forEach(function(k){
      var row = document.createElement('div');
      row.className = 'row';
      var name = document.createElement('span');
      name.className = 'k';
      name.textContent = k.label || k.name;
      name.title = k.name;
      row.appendChild(name);

      if(k.control === 'archive-default'){
        var wrap = document.createElement('span');
        wrap.className = 'arch';
        var sel = document.createElement('select');
        [['local','拷进库里'],['text-only','只留正文'],['ext','留在外部根']].forEach(function(opt){
          var o = document.createElement('option');
          o.value = opt[0]; o.textContent = opt[1]; sel.appendChild(o);
        });
        var ext = document.createElement('input');
        ext.type = 'text'; ext.placeholder = 'ext:<root>:<relpath>';
        var hidden = document.createElement('input');
        hidden.type = 'hidden'; hidden.dataset.key = k.name;
        var cur = String(k.value === null || k.value === undefined ? 'local' : k.value);
        var isExt = cur.indexOf('ext:') === 0;
        sel.value = isExt ? 'ext' : (cur === 'text-only' ? 'text-only' : 'local');
        ext.value = isExt ? cur : 'ext:';
        ext.style.display = isExt ? '' : 'none';
        hidden.dataset.orig = cur;
        function syncArch(){
          hidden.value = (sel.value === 'ext') ? (ext.value || 'ext:') : sel.value;
        }
        sel.addEventListener('change', function(){
          if(sel.value === 'ext'){ ext.style.display = ''; if(!ext.value) ext.value = 'ext:'; }
          else { ext.style.display = 'none'; }
          syncArch(); refresh();
        });
        ext.addEventListener('input', function(){ syncArch(); refresh(); });
        syncArch();
        wrap.appendChild(sel); wrap.appendChild(ext); wrap.appendChild(hidden);
        row.appendChild(wrap);
      } else {
        var input;
        if(k.readonly){
          input = document.createElement('span');
          input.className = 'ro';
          input.textContent = String(k.value);
          input.dataset.key = k.name;
          input.dataset.orig = String(k.value);
          input.dataset.readonly = '1';
        } else if(k.type === 'bool'){
          input = document.createElement('input');
          input.type = 'checkbox';
          input.checked = (k.value === true);
          input.dataset.key = k.name;
          input.dataset.orig = String(k.value === true);
        } else {
          input = document.createElement('input');
          input.type = (k.type === 'int' || k.type === 'float') ? 'number' : 'text';
          if(k.min !== null && k.min !== undefined){ input.min = k.min; }
          if(k.max !== null && k.max !== undefined){ input.max = k.max; }
          if(k.type === 'float' || k.scale){ input.step = 'any'; }
          input.value = (k.value === null || k.value === undefined) ? '' : k.value;
          input.dataset.key = k.name;
          input.dataset.orig = input.value;
        }
        input.addEventListener('input', refresh);
        row.appendChild(input);
        if(k.unit){
          var unit = document.createElement('span');
          unit.className = 'unit'; unit.textContent = k.unit;
          row.appendChild(unit);
        }
      }

      var desc = document.createElement('span');
      desc.className = 'd';
      desc.textContent = k.desc;
      row.appendChild(desc);
      box.appendChild(row);
    });
    host.appendChild(box);
  });
  refresh();
}

function changes(){
  var out = {};
  Array.prototype.forEach.call(document.querySelectorAll('[data-key]'), function(el){
    if(el.dataset.readonly === '1') return;
    var value = (el.type === 'checkbox') ? String(el.checked) : el.value;
    if(value !== el.dataset.orig){
      var spec = specOf(el.dataset.key);
      if(spec && spec.scale){ value = String(Math.round(Number(value) * spec.scale)); }
      out[el.dataset.key] = value;
    }
  });
  return out;
}

function refresh(){
  var diff = changes();
  var lines = Object.keys(diff).map(function(name){
    var spec = specOf(name);
    var value = diff[name];
    if(spec && spec.type === 'bool') value = (value === 'true') ? 'true' : 'false';
    return 'brainx config set ' + name + ' ' + value;
  });
  document.getElementById('cmds').value = lines.join('\n');
  var brief = document.querySelector('[data-key="context.max_chars"]');
  if(brief && brief.value){
    var chars = Number(brief.value) || 0;
    var layers = chars < 600 ? 'L1+L2，且很可能砍掉 L3 画像' : (chars < 1400 ? 'L1+L2+L3' : 'L1+L2+L3 且留有余量');
    cap('brief 上限 ' + chars + ' 字符 ≈ 每轮注入 ' + Math.round(chars/1.6) + ' tokens，覆盖 ' + layers
      + '。调小到 600 以下，系统看起来就会像失忆。');
  }
}

async function pickDir(){
  if(!window.showDirectoryPicker){ msg('这个浏览器不支持文件系统授权（需要 Chrome / Edge）。请复制下方命令到终端执行。', true); return; }
  try{
    var handle = await window.showDirectoryPicker({mode:'readwrite', id:'brainx-home'});
    if(handle.name !== P.homeName){
      msg('选错目录：期望 ' + P.homeName + '，实际 ' + handle.name + '。未授权。', true);
      dirHandle = null;
      return;
    }
    dirHandle = handle;
    msg('已授权目录 ' + handle.name + '（只会写 config.json 与 queue/cmd）');
  }catch(err){
    msg('授权取消：' + err.message, true);
  }
}

function validateValue(name, raw){
  var spec = specOf(name);
  if(!spec){ return '未知配置键'; }
  if(spec.type === 'int' || spec.type === 'float'){
    var num = Number(raw);
    if(!isFinite(num)){ return '必须是数字'; }
    if(spec.type === 'int' && Math.round(num) !== num){ return '必须是整数'; }
    var shown = spec.scale ? num / spec.scale : num;
    if(spec.min !== null && spec.min !== undefined && shown < spec.min){
      return '不能小于 ' + spec.min + (spec.unit ? ' ' + spec.unit : '');
    }
    if(spec.max !== null && spec.max !== undefined && shown > spec.max){
      return '不能大于 ' + spec.max + (spec.unit ? ' ' + spec.unit : '');
    }
    return '';
  }
  if(name === 'archive.default'){
    if(raw === 'local' || raw === 'text-only'){ return ''; }
    if(!/^ext:[^:]+:.+$/.test(raw)){ return '外部根格式应为 ext:<root>:<relpath>'; }
    if(raw.indexOf('..') >= 0){ return '外部根路径不能包含 ..'; }
    return '';
  }
  return '';
}

async function writeConfig(){
  var diff = changes();
  var names = Object.keys(diff);
  if(names.length === 0){ msg('没有改动。'); return; }
  for(var i=0;i<names.length;i++){
    var name = names[i];
    var err = validateValue(name, diff[name]);
    if(err){
      var spec = specOf(name);
      msg('配置未写入：' + ((spec && spec.label) || name) + '：' + err, true);
      return;
    }
  }
  if(!dirHandle){ await pickDir(); if(!dirHandle) return; }
  try{
    var fh = await dirHandle.getFileHandle('config.json');
    var original = await (await fh.getFile()).text();
    var parsed = JSON.parse(original);
    names.forEach(function(name){
      var spec = specOf(name);
      var raw = diff[name];
      if(spec.type === 'int') parsed[name] = parseInt(raw, 10);
      else if(spec.type === 'float') parsed[name] = parseFloat(raw);
      else if(spec.type === 'bool') parsed[name] = (raw === 'true');
      else parsed[name] = raw;
    });
    var writable = await fh.createWritable();
    await writable.write(JSON.stringify(parsed, null, 2));
    await writable.close();
    msg('已写入 ' + names.length + ' 个键。下次启动 brainx 会再校验一次并记一条 op=config 审计。');
    Array.prototype.forEach.call(document.querySelectorAll('[data-key]'), function(el){
      el.dataset.orig = (el.type === 'checkbox') ? String(el.checked) : el.value;
    });
    refresh();
  }catch(err){
    msg('写入失败：' + err.message, true);
  }
}

async function queueCmd(verb, ids){
  if(!dirHandle){ await pickDir(); if(!dirHandle) return; }
  try{
    var queueDir = await dirHandle.getDirectoryHandle('queue', {create:true});
    var cmdDir = await queueDir.getDirectoryHandle('cmd', {create:true});
    var name = Date.now().toString(36) + '-' + Math.random().toString(36).slice(2,8) + '-' + verb + '.json';
    var body = JSON.stringify({verb: verb, ids: ids || [], at: new Date().toISOString()});
    var fh = await cmdDir.getFileHandle(name, {create:true});
    var writable = await fh.createWritable();
    await writable.write(body);
    await writable.close();
    msg('已排入队列（' + verb + '）。下次运行任意 brainx 命令时执行一次，不是现在执行。');
  }catch(err){
    msg('排队失败：' + err.message, true);
  }
}

document.addEventListener('click', function(e){
  var btn = e.target && e.target.closest ? e.target.closest('[data-confirm]') : null;
  if(btn){ queueCmd('confirm',[btn.getAttribute('data-confirm')]); }
});
fillGroups();
</script>
</body></html>`

// pageData 是模板入参：Stats 内嵌，其余是模板友好的预渲染片段。
type pageData struct {
	store.Stats
	TodoTotal      int
	WeekDeltaText  string
	ImportMeanText string
	RecallMeanText string
	KindBars       template.HTML
	Chart          template.HTML
	BytesText      string
	Payload        template.JS
	PendingHTML    template.HTML
	ReviewHTML     template.HTML
	MostUsedHTML   template.HTML
	DecayHTML      template.HTML
	NeverHTML      template.HTML
}

// Generate 生成看板文件，返回落点与本次快照。
func Generate(s *store.Store, out string) (string, store.Stats, error) {
	stats, err := s.BuildStats()
	if err != nil {
		return "", stats, err
	}
	if out == "" {
		out = filepath.Join(s.Home, "report.html")
	}
	if err := os.MkdirAll(filepath.Dir(out), 0700); err != nil {
		return "", stats, err
	}
	payload, err := buildPayload(s)
	if err != nil {
		return "", stats, err
	}
	var bars strings.Builder
	for _, item := range stats.Kinds {
		bars.WriteString(string(bar(item)))
	}
	if bars.Len() == 0 {
		bars.WriteString(`<p class="muted">库里还没有 active 资产</p>`)
	}
	data := pageData{
		Stats:          stats,
		TodoTotal:      stats.Pending + stats.OpenReviews + stats.QueuePending,
		WeekDeltaText:  signed(stats.WeekDelta) + "%",
		ImportMeanText: fmt.Sprintf("%.1f", stats.ImportMean),
		RecallMeanText: fmt.Sprintf("%.1f", stats.RecallMean),
		KindBars:       template.HTML(bars.String()),
		Chart:          chart(stats),
		BytesText:      humanBytes(stats.DBBytes),
		Payload:        template.JS(payload),
		PendingHTML:    pendingBlock(stats.PendingGroups),
		ReviewHTML:     reviewBlock(stats.Reviews),
		MostUsedHTML:   boardBlock(stats.MostUsed, mostUsedMetric, "近 30 天没有召回记录。"),
		DecayHTML:      boardBlock(stats.NearDecay, decayMetric, "没有快被衰减掉的资产。"),
		NeverHTML:      boardBlock(stats.NeverUsed, neverMetric, "每条 active 资产都至少被召回一次。"),
	}
	file, err := os.Create(out)
	if err != nil {
		return "", stats, err
	}
	defer file.Close()
	if err := template.Must(template.New("report").Parse(page)).Execute(file, data); err != nil {
		return "", stats, err
	}
	return out, stats, nil
}

// Path 是看板的默认落点。`brainx report` 不带 --out 时用它。
func Path(s *store.Store) string { return filepath.Join(s.Home, "report.html") }

func signed(value int) string {
	if value > 0 {
		return "+" + fmt.Sprint(value)
	}
	return fmt.Sprint(value)
}

func boardBlock(rows []store.StatAsset, metric func(store.StatAsset) string, empty string) template.HTML {
	if len(rows) == 0 {
		return template.HTML(`<p class="muted">` + template.HTMLEscapeString(empty) + `</p>`)
	}
	var b strings.Builder
	b.WriteString(`<table><tr><th>标题</th><th>kind</th><th>指标</th><th>标题来源</th></tr>`)
	for _, row := range rows {
		b.WriteString(`<tr><td class="t" title="` + template.HTMLEscapeString(row.Title) + `">` +
			template.HTMLEscapeString(row.Title) + `</td><td><code>` + template.HTMLEscapeString(row.Kind) +
			`</code></td><td>` + template.HTMLEscapeString(metric(row)) + `</td><td class="muted">` +
			template.HTMLEscapeString(store.TitleSourceLabel(row.TitleSource)) + `</td></tr>`)
	}
	b.WriteString(`</table>`)
	return template.HTML(b.String())
}

func mostUsedMetric(row store.StatAsset) string {
	return fmt.Sprintf("30 天召回 %d 次", row.Recalls30)
}

func decayMetric(row store.StatAsset) string {
	return fmt.Sprintf("importance %.0f · 上次召回 %s", row.Importance, daysAgo(row.LastUsedAt))
}

func neverMetric(row store.StatAsset) string {
	return "入库 " + daysAgo(row.CreatedAt) + " · 0 次召回"
}

func pendingBlock(groups []store.PendingGroup) template.HTML {
	if len(groups) == 0 {
		return template.HTML(`<p class="muted">没有待确认资产。</p>`)
	}
	var b strings.Builder
	for _, group := range groups {
		b.WriteString(`<details><summary><span>` + template.HTMLEscapeString(group.Reason) +
			`</span><b>` + fmt.Sprint(group.Count) + `</b></summary>`)
		for _, item := range group.Items {
			b.WriteString(`<div class="item"><span class="t" title="` + template.HTMLEscapeString(item.Title) + `">` +
				template.HTMLEscapeString(item.Title) + `</span><button class="btn" data-confirm="` +
				template.HTMLEscapeString(item.ID) + `">确认</button></div>`)
		}
		if group.Count > len(group.Items) {
			b.WriteString(`<div class="muted">只列最近 8 条，其余见 brainx context --pending</div>`)
		}
		b.WriteString(`</details>`)
	}
	return template.HTML(b.String())
}

func reviewBlock(reviews []store.ReviewItem) template.HTML {
	if len(reviews) == 0 {
		return template.HTML(`<p class="muted">没有未裁决冲突。</p>`)
	}
	var b strings.Builder
	for _, item := range reviews {
		b.WriteString(`<details><summary><span>` + template.HTMLEscapeString(item.ATitle) + ` ⇄ ` +
			template.HTMLEscapeString(item.BTitle) + `</span></summary>`)
		if item.Detail != "" {
			b.WriteString(`<div class="muted">` + template.HTMLEscapeString(item.Detail) + `</div>`)
		}
		b.WriteString(`<div class="muted">裁决：<code>brainx review resolve ` + template.HTMLEscapeString(item.ID) +
			` annotate|supersede|pick_a|pick_b|merge</code></div></details>`)
	}
	return template.HTML(b.String())
}

func humanBytes(n int) string {
	switch {
	case n >= 1<<30:
		return fmt.Sprintf("%.1f GB", float64(n)/(1<<30))
	case n >= 1<<20:
		return fmt.Sprintf("%.1f MB", float64(n)/(1<<20))
	case n >= 1<<10:
		return fmt.Sprintf("%.0f KB", float64(n)/(1<<10))
	}
	return fmt.Sprintf("%d B", n)
}

// buildPayload 把 schema 与当前值打成页面用的 JSON。
// **schema 从 store 读，页面不另写一份**——两套必然漂移。
func buildPayload(s *store.Store) (string, error) {
	snapshot, err := s.ConfigSnapshot()
	if err != nil {
		return "", err
	}
	type keyPayload struct {
		Name       string   `json:"name"`
		Label      string   `json:"label"`
		Group      string   `json:"group"`
		GroupLabel string   `json:"groupLabel"`
		Type       string   `json:"type"`
		Value      any      `json:"value"`
		Min        *float64 `json:"min"`
		Max        *float64 `json:"max"`
		Desc       string   `json:"desc"`
		Readonly   bool     `json:"readonly"`
		Control    string   `json:"control,omitempty"`
		Scale      float64  `json:"scale,omitempty"`
		Unit       string   `json:"unit,omitempty"`
	}
	// 页面只渲染基础旋钮：高级键仍可 config set，但不进看板。
	var keys []keyPayload
	for _, spec := range store.BasicConfigSchema() {
		value := snapshot[spec.Name]
		min, max := spec.Min, spec.Max
		control, unit := "", ""
		scale := 0.0
		// fetch.max_bytes 在配置里是字节，页面上按 MB 展示/输入。
		if spec.Name == "fetch.max_bytes" {
			scale = 1024 * 1024
			unit = "MB"
			if f, ok := value.(float64); ok {
				value = f / scale
			}
			if min != nil {
				v := *min / scale
				min = &v
			}
			if max != nil {
				v := *max / scale
				max = &v
			}
		}
		if spec.Name == "archive.default" {
			control = "archive-default"
		}
		keys = append(keys, keyPayload{
			Name: spec.Name, Label: store.BasicKeyLabel(spec.Name),
			Group: spec.Group, GroupLabel: store.BasicGroupLabel(spec.Group),
			Type: spec.Kind, Value: value, Min: min, Max: max,
			Desc: spec.Desc, Control: control, Scale: scale, Unit: unit,
		})
	}
	encoded, err := json.Marshal(map[string]any{
		"home":     s.Home,
		"homeName": filepath.Base(s.Home),
		"keys":     keys,
	})
	if err != nil {
		return "", err
	}
	return string(encoded), nil
}
