# EasyVCS 架构设计文档

一个 **Change-Native** 版本控制系统。基于 jj(Jujutsu) 的理念，但去掉 Git 兼容层、操作日志、工作副本跟踪，聚焦于**基于 API 的修改系统 + 文件/数据库存储**。

## 1. 核心命题

**Change 是唯一的第一等实体。** 一次"逻辑修改"有永久稳定的 ID；内容（Snapshot）是可变的、内容寻址的。任何 rebase/squash/amend 都改变内容但**永不改变** change_id。

## 2. 命名与概念

| 概念 | 命名 | 说明 | 稳定性 |
|---|---|---|---|
| 一次修改 | `Change` / `change_id` | 用户引用的主概念 | **永不变** |
| 内容版本 | `Snapshot` / `sha` | 内容寻址的快照，图节点 | 会变（rebase 后新 sha） |
| 存储容器 | `repository` | 一个 `.easyvcs` 目录 / 一个 DB | — |
| 可变引用 | `bookmark` | 指向 change | repoint |
| 不可变引用 | `tag` | 指向 change | 锚点 |
| 工作副本 | 无 | API 手动提交 | — |
| 操作日志 | 无 | 不支持 undo/reflog | — |

## 3. 对象模型

```
Snapshot (= 图节点 = commit)
├─ sha          ← 内容哈希（parents+tree+desc 派生）rebase 会变
├─ change_id    ← 随机稳定标签，挂在此快照上
├─ parents[]    ← DAG 边（指向父 snapshot sha）
├─ tree_id      ← 根树（内容）
├─ description / author / time

Change (= 逻辑修改 = 稳定对象)
├─ id           ← 随机生成，永不修改
├─ current      ← 指向当前 snapshot sha
└─ created

Object (内容寻址，不可变)
├─ blob（文件内容）
├─ tree（目录：name → (kind, object sha)）
└─ conflict（合并冲突，一等对象）
```

### 关键规则
1. **change↔snapshot 1:1**：一个 change 指向唯一的 current snapshot。
2. **rebase = 换 snapshot sha，change_id 不变**：只改 parents，生成新 sha，repoint current。
3. **squash/合并 = 生成新 sha**：内容经 3-way 合并，吸收进目标 change（保留目标 change_id）。
4. **`sha` 不含 change_id**：sha 仅由 parents+tree+description 派生（jj 同款做法），change_id 独立无关。

## 4. 存储抽象（关键架构）

通过 `store.Store` 接口将**语义层**与**存储后端**解耦。同一套 change/merge 逻辑可跑在不同后端上。

```
┌─ 语义层（change 包）  与存储无关 ─────────────────────┐
│  change.Workspace: Commit / Rebase / Merge / Log     │
└────────────────────▲────────────────────────────────┘
                     │ store.Store 接口
        ┌────────────┴───────────────────┐
        │                                  │
   FileStore (本地)               SqlStore (server)
   .easyvcs/ 目录                SQLite(默认)/Postgres
   对象不可变+原子rename            database/sql, 无cgo
```

### 目录布局（FileStore）
```
<repo>/.easyvcs/
├── objects/      ← 内容寻址对象（不可变，blake3 文件名）
├── snapshots/    ← snapshot 元数据 json
├── changes/      ← change 元数据 json
└── refs/         ← bookmark/tag
```
- **objects 不可变只增**：`rsync`/`Dropbox` 同步安全（jj 同设计）。
- **元数据原子写**：`tmp + rename`，并发不会读到半截文件。

### DB Schema（SqlStore）
```
objects(sha PK, kind, content BLOB)          -- 内容寻址，可去重
snapshots(sha PK, change_id, parents, tree_id, description, author, commit_time)
changes(id PK, current, created)
refs(name PK, kind, target)
```
- SQLite/Postgres 同一 schema（标准类型 + `database/sql`），无 cgo。
- 本地用一个 `.db` 文件（SQLite 本身就是文件）；server 换 Postgres。

## 5. 后端实现选择（多态 Store）

`store.Store` 接口（internal/store/store.go）：
- `WriteObject/ReadObject/ObjectExists`
- `PutSnapshot/GetSnapshot`
- `PutChange/GetChange/UpdateChangeCurrent/ListChanges`
- `PutRef/DeleteRef/GetRef/ListRefs`
- `Init/Close`

实现：
| 实现 | 用途 |
|---|---|
| `FileStore` | 本地 `.easyvcs/` 目录（默认） |
| `SqlStore` | SQLite（`OpenSqlite`）/ Postgres（`OpenPostgres`），同接口 |

## 6. 语义操作

### Commit（产生新 snapshot [，可选新 change]）
1. 从工作区构建 tree（递归写 blob/tree 对象）。
2. 计算 `sha = blake3(change_id + tree_id + parents + desc + author)`。
3. 写 snapshot；若 change_id 为空则随机生成，否则 repoint 现有 change 的 current。

### Rebase（change_id 稳定，sha 变）
1. 读 change 的 current snapshot。
2. **保留 tree_id、desc、author**，仅替换 parents → 新 sha。
3. 写新 snapshot，repoint change.current。**change_id 永不改。**

### Merge（3-way tree merge + 一等冲突）
- 对 base/ours/theirs 三棵树的每个路径递归合并。
- 无冲突 → 合并树；有冲突 → 返回冲突列表（路径 + 参与的三方 sha），不失败。
- 冲突是**一等对象**（jj 理念），可在 tree 里记录，供后续 resolve。

## 7. CLI 命令

```
easyvcs init [DIR]                    创建仓库
easyvcs commit [DIR]                  从工作区快照（新 change）
easyvcs amend <change> [DIR]          更新现有 change
easyvcs log [DIR]                     列 change + 当前 snapshot
easyvcs show <sha> [DIR]              看 snapshot 元数据
easyvcs rebase <change> --onto <sha>  change 改父（id 不变）
easyvcs merge --base/--ours/--theirs  3-way 合并
easyvcs rev <expr>                    解析修订（@ / <change_id>）
```

## 8. API Server（easylab）

HTTP/JSON：
- `POST /commit`   `{change_id?, parents?, tree_id, description, author}`
- `POST /rebase`   `{change_id, new_parents}`
- `POST /merge`    `{base, ours, theirs}`
- `GET  /log`      列表
- `GET  /change/{id}` 单项

server 后端可用 `store.OpenPostgres` 换成 Postgres；`change`/`merge` 逻辑零改动。

## 9. 与 jj 的对比（我们简化了什么 / 保留了哪些核心）

**保留（与 jj 同等能力）**：change_id 稳定、内容寻址、3-way 合并 + 一等冲突、tree/blob 对象模型。

**简化（我们的设计取舍）**：
| 丢弃 | jj 对应 | 收益 |
|---|---|---|
| Git 兼容层 | git_backend/git 命令 (34k行) | 砍掉 |
| 操作日志/undo | op_store/operation (~2k行) | 砍掉（无 oplog） |
| 工作副本 mtime | local_working_copy (~3.2k行) | 砍掉（无实时跟踪） |
| revset 大语言 | revset.rs (6.5k行) | 精简为 @ / change_id |
| 模板/富diff/签名 | templates/gpg | 砍掉 |

**代码规模**：核心库约 1.6k 行 Go（相较 jj ~267k Rust）。保留引擎核心（对象模型、树合并、可插拔存储），砍掉网络/oplog/工作副本/Git 兼容/UX 层。

## 10. 命令集（本次补充）

```
init / commit / amend / log / show
diff <shaA> <shaB>            文件级差异（A/M/D）
checkout <sha> [DEST]         把 snapshot 的树落到文件系统
rebase <change> --onto <sha>  change 改父（change_id 稳定，sha 变）
squash <change>               change 吸收进父 change（父 change_id 稳定）
merge --base/--ours/--theirs  3-way 树合并（一等冲突）
bookmark <name> <change>      可变引用 -> change
tag <name> <change>           不可变引用 -> change
refs                          列出所有 bookmark/tag
rev <expr>                    解析修订（@ / <change_id>）
```

## 11. API（easylab，本次补充）

```
POST /commit      {change_id?, parents[], tree_id, description, author?}
POST /rebase      {change_id, new_parents[]}
POST /squash      {change_id}
POST /merge       {base, ours, theirs}
POST /ref/{name}  {kind: bookmark|tag, target}
DELETE /ref/{name}
GET  /log / /refs / /change/{id} / /diff/{a}/{b}
```

## 12. 与 jj「文件能否存 DB」的相回回应

- jj 目前只实现 `SimpleBackend`（文件）+ `GitBackend`，**没有 SQL 版**，但 `Backend` trait 预留了插槽。
- EasyVCS 从第一天就用 `Store` 接口，同提供 `FileStore` + `SqlStore`，因此**天然支持 SQLite/Postgres**。
- 由于不兼容 Git，EasyVCS 可把内容本体（tree/blob）也放进 DB，比 jj 更彻底（jj 的 Git 模式必须保持 `.git` 对象格式才不失互操）。

## 13. 构建

```
CGO_ENABLED=0 go build -o easyvcs ./cmd/easyvcs
CGO_ENABLED=0 go build -o easylab ./cmd/easylab
```

## 14. EasyLab 开发/部署平台（ops）

EasyLab 把「代码托管 + 制品仓库 + 容器构建/启动」收进一个自包含容器（`easylab`），
复用同一中央 SQLite 与鉴权。对外暴露 `/api/v1/ops/*`：

| 端点 | 说明 |
|---|---|
| `GET /api/v1/ops/namespaces` | 已批准 ops 命名空间 + 默认（`EASYVCS_OPS_NAMESPACES`） |
| `POST /api/v1/ops/runs` | 一次性命令（容器内子进程，日志流到 task） |
| `GET /api/v1/ops/tasks[/{id}][/stream]` | SSE 任务进度（有界日志 + 广播） |
| `POST /api/v1/ops/builds` | 用内置 **buildah** 构建镜像（`bud`），推送回内置 `/v2` |
| `/api/v1/ops/services` (+`/scale`,`/{name}`) | 用内置 **podman** 启动/管理服务 |

### 14.1 镜像构建（buildah，内置）
- 构建后端**只有 buildah**（daemonless、rootless、`--layers` 本地缓存 + registry 缓存复用）。
- `buildDir()` 用 `EASYVCS_BUILDAH_TMP`（默认 `<EASYVCS_HOME>/buildtmp`）作可写 ext4 目录；
  `--tls-verify=false`、`STORAGE_DRIVER=vfs`、`BUILDAH_ISOLATION=chroot`。
- 内部 `/v2`（`EASYVCS_REGISTRY`，默认 `127.0.0.1:8080`）供 buildah 拉基础镜像、缓存、推送产物。

### 14.2 服务启动（podman，内置，唯一后端）
- 服务后端**只有内部 podman**（无 docker / k8s 依赖），每服务一个 podman 容器：
  - 每服务独立 bridge 网络；`Group`/`Network` 相同时**共享网络**，服务间用 `<name>` 域名互访（aardvark DNS）。
  - 真实 cgroup 资源限制：`--cpus` / `--memory`（`ServiceRequest.CPUs`/`MemoryBytes`）。
  - 端口映射到 EasyLab loopback（`WorkerURL`），供反代/外部访问。
- 需 pod 满足：`privileged` + `capAdd[SYS_ADMIN,NET_ADMIN,NET_RAW]` + `hostUsers` + `/sys/fs/cgroup` 可写
  （entrypoint 启动时 `mount -o remount,rw /sys/fs/cgroup`，或 hostPath 挂 cgroup）。
- `ensureNetwork` 会清 aardvark-dns 残留配置，避免旧网络网关导致 DNS 服务崩溃。

### 14.3 后端选择
`easylab` 里 `newServiceRunner()` 固定返回 podman 后端；`Builder` 固定 buildah。
数据库仍为单一 `~/.easyvcs/easyvcs.db`（`EASYVCS_HOME` 覆盖）。

### 14.4 构建/运行镜像（easy-lab）
- `easy-lab/Dockerfile`：runtime 基于 `alpine`，apk 源切到阿里云镜像（`mirrors.aliyun.com`，加进 `NO_PROXY`）。
- `easy-lab/build-image.sh`：用**集群 buildkitd**（`buildctl`）构建 EasyLab 镜像 → 导出 docker archive →
  `skopeo` 推送到 forgejo OCI（`forgejo.../root/easylab:v0.1.0`）。
- EasyLab 容器内**只内嵌 buildah + podman**（不做 Docker-in-Docker，无 docker-cli）。

### 14.5 可切换后端（为横扩/集群化铺路）

EasyLab 的存储边界已抽象为可切换，语义层（revision/merge/transfer/pkrkit）零改动：

| 层 | 接口 | 后端开关 |
|---|---|---|
| 元数据 | `CentralStore` | `EASYVCS_DB_DRIVER=sqlite\|postgres`，`EASYVCS_DB_DSN` |
| 对象/blob | pkrkit `BlobStore` | `EASYVCS_BLOB_BACKEND=sqlite\|s3`（s3 为占位） |

- `internal/store/driver.go`：`DriverConfig{Kind,DSN}` + `OpenDriver`；`rebindPostgres` 把 `?`→`$n`；
  `SetWAL`/`migrateRepoColumns` 按 kind 分支。
- `cmd/easylab/main.go` 用 `OpenDriver` 组装；`cmd/easylab/registry.go` 按 `BLOB_BACKEND` 选 blob。
- 从单容器升级到集群只需：`EASYVCS_DB_DRIVER=postgres`（元数据共享）+ `EASYVCS_BLOB_BACKEND=s3`（对象共享）。

### 14.6 Agent 原生：extension 桥接（easyvcs 完全独立于 agent）

easylab **不依赖任何 agent 协议**，只暴露普通 HTTP `/api/v1`。agent 能力由两个独立
extension 进程桥接（各自 `Config.ID`，通过 NATS `discover`/`tool.call` 被 agent 动态发现）：

```
zergx-agent / 任意 agent
   │  NATS: abc.discover / tool.call.{extId}.{tool}   (nats://127.0.0.1:14222, JetStream)
   ├─ easyvcs-code   (11 tools, 读/写代码 → revision 落库)
   └─ easyvcs-ops    (9 tools, sandbox / build / 服务)
   (两个 extension 只调 easylab 的普通 HTTP /api/v1)
easylab (核心, 纯 VCS/ops, 与 agent 无关 · :18160)
nats-server (JetStream · :14222)   ← 内嵌消息/事件总线
```

- **`easy-lab/ext/code/`** → `easyvcs-code`：`list read write edit delete search revision_diff log blame refs history`。
- **`easy-lab/ext/ops/`** → `easyvcs-ops`：`sandbox_run sandbox_read sandbox_write sandbox_services container_build service_deploy service_scale service_status service_delete`。
- 走 `abc-protocol/sdk-go`（`easy-lab/deps/abc-sdk-go`，本地 replace）。协议常量全部 `abc.*`，
  无产品名绑定；agent 不写死应用名。
- easylab 新增 `/api/v1/ops/sandbox/{name}/{exec,file}`（持久 podman 容器内执行/读写，**不落库**）。
- 动态插拔：起停 `easyvcs-code`/`easyvcs-ops` 程序组即可，agent 下次 `discover` 自动感知。
- supervisord 组：`nats`、`easyvcs-code`(:18091)、`easyvcs-ops`(:18092)、`easylab`(:18160)，全部常驻。

未开放（按决策后置）：历史重写（rebase/squash/resolve/merge）、MR、发布(pkrkit package)、browser/helm。


