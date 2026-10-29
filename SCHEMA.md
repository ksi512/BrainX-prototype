# BrainX 数据模型

核心存储为 `store.db`，开启 SQLite WAL、foreign keys 和 `synchronous=NORMAL`。表结构包含 `assets`、`asset_tags`、`chunks`、`chunks_fts`、`edges`、`runs`、`events`、`reviews`、`recalls`、`recall_hits` 与 `meta`。

`assets` 同时表示记忆和知识：`kind` 区分 `memory`、`fact`、`skill`、`profile`、`note`、`doc`、`synthesis`、`insight`；`scope` 隔离上下文；`content_hash` 用于去重与审计。

## 可变性契约

- `doc` 创建时始终为 `immutable=1`、`layer=0`。正文是 ground truth，巩固、合并和删除都不能改写它。
- `synthesis` 与 `insight` 必须通过 `derived_from` 边保留来源；`insight` 应以 `pending` 状态等待人工确认。
- 其他记忆资产可以按审计决策变更或归档。
- `files/` 的库内引用必须保存为相对于 `$BRAINX_HOME` 的路径；外部归档采用 `ext:<root>:<relpath>`。

迁移时复制整个 `$BRAINX_HOME`，或使用 `brainx export out.mbz` 生成包含数据库和文件的压缩包。

## 两条维护须知

**FTS 落在 chunk 级，且是 contentless 表。**

`chunks_fts` 用 `content=''`，索引的是**已切好的 bigram 文本**。这条设计让长文档能命中具体段落（asset 级索引会退化成「整篇命中」），代价是**删行不能用 `DELETE`**：

```sql
-- 正确：带上写入时的原始 token
INSERT INTO chunks_fts(chunks_fts, rowid, title_ngram, text_ngram, asset_id, seq)
VALUES('delete', ?, ?, ?, ?, ?);
```

漏掉这一步，旧词会永远留在索引里形成幽灵命中（`update` 之后「旧词还能查到」）。`store.Update` 已按此实现并有常驻测试守着。

**scope 标识由 `internal/scope` 统一判定。**

从 cwd 向上逐级找，遇 `.git` 即停；标识取 `remote origin` URL 的 sha256 前 12 位，无 remote 时取仓库目录名。**禁止用 cwd**（子目录里查不到根目录记的东西＝静默失忆），**禁止用绝对路径**（换机器/换用户名即断链）。读操作不给 `--scope` 时自动判定并把依据打到 stderr；写操作必须显式给。

## 召回日志与看板口径

`recalls` 一行 = **一次读**（`brainx recall` 查询，或 `context` / hook 注入）；`recall_hits` 一行 = 一次命中。

为什么必须单独立表：`use_count` 是**累计值**，一条三年前的老记忆天然高于上周的新记忆，拿它排「近 30 天最常用」等于排「谁先入库」。看板的两条曲线与三条资产榜全部按时间窗从这两张表算。

口径写死在这里，改口径必须同时改 `internal/store/stats.go`：

| 指标 | 口径 |
|---|---|
| 每日召回（折线） | `recalls` 当天全部行，**含注入**。排除注入会让重度用 hook 的人看到一条恒为 0 的线 |
| 30 天召回次数 / 零命中 | 只算 `query` 非空的查询行；零命中是缺口信号，注入没有命中与否之分 |
| 近 30 天最常用 | `recall_hits` 时间窗内命中次数降序 |
| 快被衰减杀掉 | `importance≥50` 且 ≥60 天未召回，且 `layer>0`、`kind<>'doc'`（doc 永不衰减，两处口径必须一致） |
| 从未被召回 | `use_count=0` 且入库 >7 天，附 `title_source` |

`assets.title_source` 记录标题是谁给的（`flag` / `filename` / `body-first-line` / `agent`）。它是「存了但搜不到」的排查入口：自动回落的标题往往就是搜不到的根因。该列由 `store.Init` 的 `addColumnIfMissing` 幂等补齐，旧库无需手工迁移。

## queue/ 与 queue/cmd/ 是两套语义

| 目录 | 是什么 | 失败怎么办 |
|---|---|---|
| `queue/` | **丢了的数据**（写锁竞争时的补偿记录） | 重试三次，仍失败进 `queue/dead/`，`doctor --retry-dead` 人工放回 |
| `queue/cmd/` | **命令**（看板按钮排队） | 执行一次即删，**失败不重试**（会失败的命令重试一百次还是失败，只会拖慢每次启动） |

`DrainQueue` **只扫 `queue/` 本层**。把它一起扫，一条 UI 命令会被当成丢写反复重放，还会被改写成补偿记录的格式把命令本身弄坏——这条有常驻测试守着。

## 配置审计在启动时做，不在写入时做

看板通过 File System Access 直写 `config.json`，绕过了 CLI 的校验函数，所以 CLI 不可能在写入瞬间记账。改为：`meta.config:last_content` 存上次已知的全文，启动时比对，有差异就写一条 `events(op='config')`，`reason` 是 `{key,old,new}[]`。

`SaveConfig` 会同步推进这个基线，所以 `brainx config set` 自己写的改动不会被再审计一遍。代价是记不到「谁改的」——个人单机场景不需要，也拿不到。

**防线②**（`reportConfigDrift`）：文件里的越界值会被拉回默认，并把**具体哪个键**打到 stderr。静默修正比报错更难查——文件看起来完全正常，只是「brainx 今天不太对劲」。
