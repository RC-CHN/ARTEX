# 实验性远程 Runner

此功能默认关闭。关闭时保留原有本机工具、MCP 和文件执行方式；开启后可按 Agent 类型显式选择 `local` 或 `remote`。远程执行分配失败不会自动降级到本机。

Agent 的 LLM 循环、规划、审批、业务数据库和界面仍在 ARTEX 控制端。一个 Agent 线程的一次执行绑定一个 runner 沙箱；一个 runner 可同时承载多个线程。命令、文件工具、后台任务、交互 Shell、WebFetch、联网搜索、自定义工具和 runner 位置的 MCP 在沙箱中执行。容器按线程隔离 HOME、临时目录、进程和资源。

## 接入独立主机

执行器支持 Linux Docker 主机和 Kubernetes 原生 Pod 池。独立主机不要求加入 Kubernetes；集群外 GPU 服务器与普通 CPU 主机使用同一注册协议。控制端可以继续部署在 Kubernetes 中。远程路径映射按 Linux 绝对路径设计；Windows 控制端先保留本机模式。

1. 在执行主机安装 Docker。NVIDIA GPU 主机还需安装兼容驱动和 NVIDIA Container Toolkit，并验证 Docker 能访问指定 GPU。
2. 从本项目构建或发行包取得 `artex-runner`，放入 `/usr/local/bin/`。源码构建：`go build -o artex-runner ./cmd/artex-runner`。
3. 复制 `deploy/runner/runner.example.json`，配置 ARTEX 的 HTTPS 地址、节点名称、持久数据目录和可分配资源。资源表示允许 ARTEX 使用的容量，而非必须占满宿主机。
4. 在“系统 → Runner（实验）”配置默认执行环境镜像的**固定摘要**，启用功能，生成一次性注册令牌。运行镜像必须包含 `/usr/local/bin/artex-runner`；本项目镜像包含该文件和现有 Node、Python、Playwright 工具环境。
5. 将令牌存入仅本用户可读的临时文件，启动：`artex-runner -config runner.json -token-file enrollment.token`。注册成功后删除令牌文件。以后使用数据目录中的节点身份自动重连，无需再次注册。

Runner 主动通过 HTTPS 领取执行、续租、回传结果和传输文件，不需要开放 runner 入站端口。执行主机需要能够访问 ARTEX 入口及所配置镜像的下载入口。内网镜像入口由部署环境配置，不写入公共源码。反向代理需转发 `/runner/v1/`，允许长轮询及较大的流式文件请求；不要额外套浏览器登录跳转。注册与节点请求分别使用一次性令牌及节点凭据认证。

私有镜像仓库需要为 runner 服务账号配置一次 Docker 登录凭据或凭据助手。Docker 使用该账号的凭据拉镜像；节点注册不会自动复制控制端的 Docker 登录信息。

长期运行可使用 `deploy/runner/artex-runner.service`。先创建 `artex-runner` 用户和组，使其能够使用 Docker，配置放在 `/etc/artex-runner/runner.json`。首次注册可以通过权限为 `0600` 的 `/etc/artex-runner/enrollment.env` 提供 `ARTEX_RUNNER_ENROLLMENT_TOKEN`，注册后删除此文件。`/var/lib/artex-runner` 必须保留在持久磁盘；节点身份文件权限为 `0600`，运行缓存与未同步产物也在该目录。

`unsafe_process` 只用于显式开发测试，不提供文件或 GPU 挂载，也不构成系统级隔离。正常部署使用 Docker 或原生 Pod。明文 HTTP 仅在显式 `allow_insecure_http` 开发配置下可用。

## 容器化管理器与动态执行池

一个常驻 Runner 管理进程可以按需创建多个执行环境，无需预置两个常驻 worker。每次执行使用新的容器/Pod、HOME、临时目录和进程，系统根目录只读。正式任务文件是有意保留的产物。正常结束确认进程停止并完成最终同步后删除私有会话缓存；异常、冲突或取消时保留缓存供恢复，后续执行不会复用该目录。失败缓存目前需管理员确认无恢复需要后清理，不自动删除未同步结果。

“系统 → Runner”可查看节点/动态池、已预留执行数、CPU/内存占用与硬容量，并在线设置**并发执行环境上限、CPU 总额度和内存总额度**。每项填写 0 表示继承节点配置中对应的 `capabilities` 硬上限，不能超过硬上限。达到额度后等待容量，不会自动退回本机。调低额度只影响后续分配，已有线程保持原资源继续执行。限额写入控制端数据库，节点重连和控制端重启保留。正在创建、运行及等待清理确认的执行占用容量。

中央环境的 `resources` 控制每个新执行的 CPU/内存额度，节点额度控制同一 Runner 的总预留量。环境配置保存后同样只影响后续执行。“系统 → 系统设置”的每任务工作 Agent 数现在会热调整正在运行的工作池：增加立即补充线程，减少时让正在处理的意图完成，再收缩领取新意图的容量。任务列表中的同时运行任务上限也可在线调整，降低时保留已经运行的任务。

### Docker Compose / 外部 Docker 主机

默认 Compose 提供可选的 `runners` profile，其中只有一个 `runner-manager`。主服务实验开关仍默认关闭：

1. 升级到包含 Runner 的镜像；在页面配置执行镜像的固定摘要并启用实验功能。初次接入可先保持默认 `local`。
2. 生成一次性令牌，写入 `.env` 中 `ARTEX_RUNNER_TOKEN_FILE` 指向的私有文件。管理器需要读取它；不要提交此文件。
3. `docker compose --profile runners up -d runner-manager`。
4. 节点在线后配置页面的并发上限并切换需要的 Agent 为 `remote`。首次注册后可清空令牌文件；Compose 仍需保留这个空文件用于挂载。

`deploy/runner/compose.json` 默认硬容量是 4 个执行环境、4 核 CPU、8 GiB 内存，可按实际资源修改。`.env` 的 `ARTEX_RUNNER_DATA_DIR` 必须是 Docker daemon 主机上的绝对路径；每套部署使用独立路径/网络。容器内路径通过 `docker_host_data_dir` 映射到该宿主路径，避免 sibling 容器误挂载管理器自身的路径。任务容器只挂载私有会话子目录，不挂载整个数据目录、Docker socket、注册凭据或 Docker 登录配置。

管理器容器挂载 Docker socket，因此具有宿主 Docker 管理权限；任务隔离由执行沙箱提供。禁止给任务容器挂载这个 socket。私有仓库可用 `docker compose exec runner-manager docker login REGISTRY`，凭据保存在管理器持久数据目录的 `docker-config/`，任务不会收到。

Compose 执行容器加入独立的 `artex-runners` 网络，可访问 `artex:8787`。需要控制端录制代理时，将中央执行环境的 `proxy_url` 配成 `http://artex:8788`；明确不录制则配置 `direct_network=true`。外部机器改用实际可达的 HTTPS 控制端地址及网络入口。

可用 `docker build --target runner-manager -t artex-runner:local .` 构建只包含管理程序和客户端的镜像。正式发布的 ARTEX 镜像同样包含所需程序，通过覆盖 entrypoint 即可运行管理器；管理镜像和执行工具镜像可以分别选择。

### K3s / Kubernetes 原生 Pod 池

`deploy/runner/k8s/` 是可选模板；启用时只运行一个管理 Pod，每个 Agent 执行按需创建独立 Pod，不挂载宿主 socket，也不运行特权 Docker sidecar。

- 用私有 Kustomize overlay 修改管理镜像、ConfigMap 内 HTTPS `server_url`、两个 PVC 的共享存储类。默认独立命名空间是 `artex-runners`，如改名还要同步 JSON 配置中的 `kubernetes.namespace`。
- 两个 PVC 均使用支持 RWX 的集群共享存储，分别保存节点身份与工作目录。任务 Pod 只通过 `subPath` 获取自己的缓存，身份卷不出现在任务 Pod 中。禁止将管理 Deployment 扩容；新增独立池需分配独立身份、工作目录与配置。
- 创建该命名空间后，准备私有镜像所需的 `registry-pull` Secret。其他命名空间的 Secret 不会自动复制过来。
- 页面生成令牌后，通过 `kubectl -n artex-runners create secret generic artex-runner-enrollment --from-file=token=私有令牌文件` 保存。应用私有 overlay；节点上线后可以删除该一次性 Secret，持久凭据支持以后重启。
- 页面将执行镜像设置成固定摘要并选择环境网络出口，再切换 Agent 模式。执行镜像地址也必须是集群可达入口；Kustomize 的 `images` 不会改写数据库里的执行镜像。

使用 Harbor 等代理缓存时，要验证固定的摘要已经保存在内网仓库。缓存可能只保留已拉取的平台，并生成与上游不同的多架构索引；上游健康时原索引可被转发，上游不可用后却可能返回 `not found`。单一架构集群可从经过验证的发布索引中选择对应平台的镜像摘要，核对内网清单内容与摘要一致后，分别配置主服务、管理器和数据库中的执行环境。混合架构集群应完整同步所需平台及索引，或为不同 Runner 配置对应架构的镜像，不能把 AMD64 摘要直接用于 ARM64 主机。

升级验证应覆盖每个可调度节点的实际镜像拉取与最小容器启动，并检查代理缓存的本地清单是否完整。仅在已缓存镜像的一个节点上看到 Pod 就绪，不能证明其他节点或上游故障时也能启动。修改主服务和管理器的 Deployment 会重建容器，应等待活动执行归零；只修改中央执行环境镜像则作用于后续领取。

模板默认硬上限为 **4 个执行 Pod**，ResourceQuota 额外允许 **1 个管理 Pod**。扩大硬上限时同时修改 `capabilities.slots`、CPU/内存容量及 ResourceQuota；平时在页面调整并发及资源额度即可。执行器在互斥区内核对实际 Pod 数和资源预留后创建，Pending/Terminating Pod 也计数；API 不可达时停止新增。不会用强制删除来假装旧进程已经结束。

Pod 使用非 root 身份、只读根文件系统、禁止提权、移除 capabilities、独立临时卷、CPU/内存配额，并禁止挂载 ServiceAccount token。管理器只有专用命名空间内的 Pod 创建/查询/删除/attach 权限。心跳独立于工具调用及文件同步，管理器失联后沙箱在 30 秒内停止工具；管理器重启先清理自己的遗留 Pod，再领取任务。

原生 Pod 池目前面向 CPU 任务；GPU 继续通过外部 Docker Runner 按设备分配。Kubernetes 的 PID 上限由 kubelet `podPidsLimit` 设置，不能用 Pod 资源字段逐线程指定；中央 `resources.pids` 仅由 Docker 执行器逐容器落实。容器与 Pod 共享宿主内核，不等同于虚拟机隔离。

## 可选的 Kubernetes 预热

在「系统 → Runner → 执行设置」设置 **Kubernetes 预热 Pod 上限**，对应环境配置的 `warm_idle`（0–32，默认 0 关闭）。保存后持久化到数据库，在线生效，无须重启管理器；节点卡片显示预热就绪、准备中、实际 Pod 数量及分配资源。外部 Docker Runner 保持原有执行方式。

预热使用与冷启动完全相同的 Pod 定义。Kubernetes 的工作目录、环境变量和挂载不能在领取时任意更换，因此控制器根据待执行任务的完整配置提前准备；没有待执行布局时，不创建通用空壳 Pod。领取时严格匹配镜像、资源、环境变量、真实工作目录及读写挂载，不用符号链接替代挂载。未命中时启动同样配置的新 Pod，并把 Pod 启动与输入同步并行进行；MCP、工具和 Agent 执行始终等输入同步完成后才开始。任务文件不会在领取前下载。

就绪和准备中的预热 Pod 都占用节点数量、CPU、内存预算，Pending/Terminating Pod 也计入物理容量。降低额度只影响后续分配并回收闲置 Pod，现有执行不改 cgroup 配额。不同配置的执行可回收闲置预热 Pod 腾出容量。使用后的 Pod 直接销毁，不回池复用。控制器断联、配置失效或管理器重启会清理未使用 Pod；无法确认删除时保留占用并重试，避免错误释放资源。

长期运行时，已结束、停止已确认、结果已被控制器确认的命令账本自动回收；有效租约、未确认回执和未停止环境继续保留。未进入工具/MCP 初始化的输入缓存可清理；产生过执行、文件冲突或未知结果的缓存保留供恢复，不按年龄擅自删除。重新启动时只回收明确标记为未使用且没有任务文件的预热缓存。

Runner 状态快照以 PostgreSQL `JSON` 原样保存，升级时自动将旧 `JSONB` 列迁移；工具输出中的 NUL 转义不会被删除或改写。该快照只按整体读取，不使用 JSON 运算符解析。回执发送失败时 Runner 日志会显示命令 ID、重试状态和错误类别；命令结果保留重传，禁止通过重放工具来补回执。控制器同时记录状态持久化错误，避免把数据库错误误判为工具或文件同步卡死。

### 心跳与持久化诊断

使用现有管理 API 认证访问 `GET /api/runners/diagnostics`。该接口只读取独立内存计数，不获取执行主锁，也不查询数据库；检查点保存阻塞时仍可查看 `in_flight`、`waiting`、最近/最大锁等待与操作耗时、最近成功/失败时间、连续失败次数和检查点字节数。`checkpoint_marshal`、`checkpoint_store`、`checkpoint_rollback` 分别对应序列化、数据库保存和失败回滚，`store_guard` 对应控制器版本校验。计数随控制器重启清零；租约过期计数记录内存中观察到的过期转换，保存失败回滚后再次观察到过期可能重复计数。

相关日志均使用固定错误码，不记录请求正文、响应错误文本、SQL、URL、凭据或工具输入输出：

- `runner_transport`：管理器的 heartbeat/poll/results 请求耗时、HTTP 状态、错误分类、距上次成功的时间和恢复事件。同类持续异常每 30 秒汇报一次，首次失败、错误类别变化及恢复立即汇报；长轮询低于 15 秒不作为慢请求。
- `runner_broker`：控制器的锁等待、序列化/保存/回滚耗时、状态大小与租约过期事件。会话 ID、节点 ID 是运行元数据。
- `runner_store`：保存失败或超过 1 秒时，记录事务阶段（diff/begin/checkpoint_lock/manifest/write_parts/delete_parts/diagnostics/commit）、SQLSTATE、耗时、总字节数及实际写入块数/字节数。慢写入每秒至多报告一次已完成批次；阶段耗时是本次保存开始以来的累计值，不记录块名或内容。
- `runner_lease`：管理器停止会话时记录固定原因码，以及心跳、轮询最近成功时间和失败状态。

数据库不可用或超时返回 HTTP 503（`store_unavailable` / `store_timeout`），不会再被当作 HTTP 400 协议不兼容而关闭诊断字段。控制器仍兼容旧 Runner，Runner 也保留对旧控制器 400 响应的可选字段回退。HTTP 请求取消后，尚未获得锁的认证、心跳、轮询、结果提交及文件授权请求退出队列，不再补做过时的续租；已开始的状态变更仍必须完成持久化或回滚。结果回执继续重传去重，不重放工具。真正租约过期、控制器接管、停止与取消的隔离语义保持不变。

状态加载和保存失败后的回滚统一重建可写 map，保留通知与工具调用的去重记录；空 map 被 JSON 省略后也能继续接收通知和调用。会话续租同时检查自身到期时间，不只依赖节点最近活跃时间，避免未完成的轮询让后续心跳误判为已续租。轮询/心跳状态操作发生 panic 时，在释放状态锁前恢复最后确认保存的状态，返回 HTTP 500（`internal_panic`），并记录 `operation_panic` 及独立 `incident_id`。记录不包含 panic 原文或请求内容；`recovered` 表示本次内存回滚成功，不表示工具已重跑。恢复失败或涉及未完成的旧进程隔离时关闭 Broker，保留人工处理要求；保存结果不确定时仍通过 revision 校验防止继续使用陈旧状态。

`GET /api/runners/sessions` 的 `reason_codes` 按原始故障出现顺序返回固定原因码，`reason_code` 是其中最早的原因；后续清理失败或租约过期不再覆盖起因。两者均不返回原始错误文本。

排查成批租约过期时，应对照同一时间窗的 `runner_transport` 和 `runner_broker`：较高的 `authenticate`/`heartbeat` 锁等待伴随 `checkpoint_store` 超时支持数据库保存阻塞的判断；较高的 `checkpoint_marshal`/`checkpoint_rollback` 耗时支持历史状态处理开销的判断；管理器网络错误而控制器无对应操作时，应同时检查 HTTP handler panic 和网络路径。频繁 PostgreSQL WAL 检查点本身不能证明心跳中断的最终根因。

### 增量检查点与写入预算

v0.12.4 起，节点、会话元数据、执行配置、通知和每条命令回执分别保存为 `runner_state_parts` 中的不透明字节块；超过 1 MiB 的单项继续拆块。保存时对比 SHA-256 清单，只传输、更新有变化的块并删除过期块。租约心跳不再重写完整的工具输出历史；内存中的完整状态序列化和差异扫描仍然存在。二进制转义与大整数不经 JSONB 或浮点数转换。

块变更、revision CAS 与内容脱敏的诊断投影仍在同一 PostgreSQL 事务提交，读取采用一致性快照；失败整笔回滚，提交结果不明时通过后续 CAS 检出，不自动重放工具。连接、锁等待、写入或提交连续 **5 秒没有完成进展**就取消；每个已完成批次刷新该预算，整个保存最多 **2 分钟**。有活跃会话时，总预算进一步限制为最早已提交租约的剩余时间减去续租余量（默认 5 秒），即将到期时仅允许短尝试；待提交的新租约不能扩大这个预算。调用方取消/截止时间优先。Runner 心跳请求允许最多等待 15 秒，租约 watchdog 保持独立。

旧完整 JSON 在下一次保存时原子转为分块格式，无需重跑任务。主记录使用不受旧版本支持的版本标记，旧二进制会拒绝不完整的执行历史。**降级到 v0.12.3 或更早版本前**，须等待任务和租约空闲、停止控制器和管理器，使用新版本二进制及原数据库连接配置执行 `artex -runner-checkpoint-legacy`；成功后再启动旧版本。转换只改变 Runner 检查点表示、推进 revision 并清理分块，不恢复旧任务备份、不输出任务内容；忙碌集群会拒绝转换。

### Broker 故障与恢复

远程 Worker 领取意图前检查 Broker 是否可用。Broker 关闭、缺失或配置加载失败时，尚未领取的意图保持 `open`。领取后才遇到故障的竞态由准备阶段处理：只有明确尚未开始模型/工具执行的环境分配失败才允许重试，保持当前意图为 `running`，每 5 秒重试，等待提示最多每 30 秒重复一次。暂停、取消和任务截止时间仍可终止等待。已经执行的工具或结果未知的会话不会因 Broker 恢复而自动重放，也不会自动重跑之前被标记为 `blocked` 的任务。显式配置为本机执行的 Agent 不受远程 Broker 关闭影响；配置本身加载失败时仍拒绝执行。

停止/隔离状态持久化失败导致的 Broker 关闭，会先冷却 5 秒，再尝试从数据库建立新的 Broker。失败后退避 10、20、40、60 秒，后续最多每分钟尝试一次。恢复与配置保存、进程关闭互斥，恢复 I/O 不占用 API 状态快照锁。新 Broker 隔离所有旧会话，保留旧租约占用直到确认清理或到期；旧 Session 句柄不会转接到新 Broker。恢复前校验原控制器 epoch，写入时继续校验状态 revision，避免抢占已经接管的控制器。

版本冲突、回滚解析失败、主动关闭/禁用以及无法读取有效状态等情况不自动接管。恢复尝试遇到所有权冲突或非存储错误也停止重试，管理员应检查原因后重新保存 Runner 配置。进程启动时的配置/状态初始化失败同样需要人工修复。`runner_broker_closed` 记录最初关闭原因、固定错误码、是否允许自动恢复和 revision；`runner_recovery` 记录尝试次数、连续失败和恢复结果。`GET /api/runners/diagnostics` 新增 `state` 和 `recovery`，不返回原始错误文本或任务数据。

`GET /api/health` 在启用的 Broker 缺失/关闭、配置加载失败或服务器退出时返回 HTTP 503，并提供 `execution.ready`、固定 `code` 和恢复状态；健康时返回 200。此检查反映控制器执行服务状态，不承诺节点在线、配额充足或数据库实时可用。独立的 `GET /api/livez` 只检查 HTTP 进程存活。Kubernetes 模板将 startup/liveness 改为 `/api/livez`，readiness 保留 `/api/auth/status` 的数据库检查，使 Broker 故障期间心跳、结果提交和修复界面仍可到达控制器。部署已有集群时需要同步更新探针路径，不能继续把 `/api/health` 用作 liveness。

### 独立故障记录

控制器把运行元数据写入 `<dataDir>/diagnostics/runner-incidents.jsonl`（标准容器为 `/app/data/diagnostics/runner-incidents.jsonl`）。该路径使用控制器持久数据卷，不写入任务目录，也不依赖 PostgreSQL 或普通服务器日志表。单文件上限 8 MiB，保留 `.1`～`.4` 四个轮转文件，总上限约 40 MiB；文件权限为 0600。每个控制器数据目录由一个进程写入，Kubernetes 使用 Recreate 升级策略。

记录覆盖控制器/Broker 启动、首次/变化的操作错误、慢操作、租约过期、Broker 关闭及每次自动恢复结果。`broker_id` 关联同一控制器执行实例的前后事件；`incident_id` 将一次关闭和恢复尝试关联起来，恢复成功包含新 Broker 的 `related_broker_id`。关闭事件附带独立内存诊断快照。所有字段均为显式元数据，不保存原始错误文本、SQL、凭据、提示词、任务正文或工具输入输出。

经管理 API 认证访问 `GET /api/runners/incidents?limit=100&incident_id=…`；`limit` 支持 1～500，`incident_id` 可省略。返回最近记录及 `journal` 写入状态，即使 Broker 初始化失败也可查询。内存最多保留 500 条；启动时从轮转日志的有界尾部恢复历史，更早记录可在上述独立文件中排查。`GET /api/runners/diagnostics` 同时提供日志路径、待写数量、累计丢弃数、写入失败次数和最后成功写入时间。

记录通过 1024 项有界队列异步追加并 fsync，不让磁盘延迟占用 Broker 主锁。存储失败每秒重试，队列满时显式计入 `dropped`；后续持久记录附带当时的 `journal_dropped_total`。文件写入失败另发固定码 `runner_journal` 日志。统计按进程计数，历史文件有容量上限，这不是无损审计系统；若持久卷持续不可写且进程退出，未落盘记录可能丢失。退出最多等待 5 秒刷新；不确定的写入重试可能产生相同 ID，读取历史时去重。崩溃留下的残缺尾行会被隔开并跳过，避免吞掉后续记录。

回归覆盖 10,000 条历史会话下的 1,000 次并发轮询（无变化时 0 次状态重写）、模拟超过 24 小时的历史过期、100,000 条回执反复回收、64 路并发领取，以及连续创建/取消/配额变更。开发验证额外重复 25 轮 race 压力测试，累计 250 万条回执与 750 次预热生命周期；这是加速堆积测试，不等同于连续运行数天的观测。真实 K3s 测试通过 `TestManagedRunnerEnvironments` 交替冷启动/预热，校验 Pod UID、物理 cwd、读写权限、共享文件、MCP、干净 HOME/tmp、cgroup 和最终零执行 Pod。

## 中央环境与节点选择

逻辑 slot、执行池 Pod 数与 Kubernetes 命名空间 ResourceQuota 是不同的计数。命名空间还包含管理 Pod；Pod 删除完成后，配额回收也可能尚未完成。创建 Pod 收到明确的 `Forbidden: exceeded quota` 时，管理器保留当前准备请求并指数退避（0.5 秒起，最多 5 秒），多个请求共用创建退避，避免并发重试冲击 API。退避不持有创建锁，等待者可以取消。权限拒绝、镜像失败、创建结果不确定不会被当成配额不足重试。

`acquire_timeout_seconds` 仍限制每次分配。若到期时确认仍在等逻辑 slot、物理 Pod 容量或配额，返回具名的 `runner_capacity_unavailable`，关闭本次准备并保留清理占用；Worker 保持同一意图为 running，5 秒后再次准备，等待旧租约清理后才分配新执行。它不会把这一意图标为 blocked 后继续消耗后续意图，也不会调用模型或重放已执行工具。人工暂停、取消、Planner 停止和任务截止时间继续生效。其他 Agent 的调用方仍收到明确的分配错误。

Runner 卡片展示准备阶段、最长准备时长、创建尝试数和管理器本次启动以来的配额拒绝次数，并显示最近拒绝时的申请量、已用量与上限；该表是带时间戳的历史快照，并非实时命名空间用量。`GET /api/runners` 的 `pool_status.admission` 提供相同元数据；`GET /api/runners/sessions?task_id=…&intent_id=…` 提供会话创建时间、准备阶段、报告时间及固定原因码。阶段变化通过独立心跳传递，旧长轮询不会覆盖更新的报告。字段在协议协商后发送，支持滚动升级和回滚。日志使用 `runner_admission` / `runner_capacity_wait` 记录等待、恢复和重试，不附带任务正文、命令或文件路径。

中央配置包括：

- `enabled`：实验功能开关，默认 `false`。
- `default_mode`：`remote` 或 `local`。
- `agent_modes`：按 Agent key 覆盖模式，例如 `{"planner":"local","worker":"remote"}`。
- `default_environment`：默认环境名称。
- `environments`：环境的镜像摘要、CPU 毫核、内存 MiB、进程数、GPU 数量、标签、显式环境变量和只读输入目录。
- `acquire_timeout_seconds`：等待容量及初始化沙箱的时限，不是 Agent 的总执行时间。
- `mcp`：个别 MCP 的执行位置及中央覆盖配置。

Worker 的单次执行总时限在 Agent 配置的“运行时长（秒）”中设置，对应 `run_seconds`；设为 `0` 即不限时，并持久保存。它与 `acquire_timeout_seconds`、单个工具的超时、Runner 心跳租约和手动取消独立。修改后用于后续执行，已经启动的执行保留启动时的截止时间；任务自身显式设置的截止时间仍然有效。

Runner 的 `capabilities.environments` 声明它接收的环境名称，`labels` 表示网络位置或额外软件能力。默认继承控制端镜像；`allowed_images` 可以限定允许的摘要，`image_override` 可以显式选择节点自己的固定镜像。覆盖镜像也必须包含 sandbox 可执行文件及所需依赖。节点配置变化需要重启 runner 后重新报告能力。

GPU 主机在 `capabilities.gpus` 中配置可分配设备，例如 `{"id":"0","model":"example-gpu","memory_mib":24576}`。设备数量、型号和显存用于匹配；首版按整张设备独占预留，显存声明不是通用显存硬配额。一张 GPU 正在执行时，同节点仍可接收容量允许的 CPU 线程。

启用后，Planner/编排 Agent 可调用 `list_runners`，查看在线状态、环境、标签、资源与已占用容量。`add_intent` 的单条与批量参数都支持：

```json
{
  "summary": "在合适的计算环境中处理输入数据",
  "runner": {
    "environment": "gpu",
    "labels": {"network": "external"},
    "resources": {"gpu_count": 1, "memory_mib": 4096}
  }
}
```

可以指定 `node_id`，也可以只声明能力让调度器选择。线程的需求保存在意图中；任务内并发上限和节点物理容量共同生效。Bash 中临时出现 GPU 命令不会自动迁移正在运行的线程，应在创建该线程时声明 GPU 需求。

## 管理界面、提示词与预装工具

“系统 → Runner”在资源概览中分别显示 Kubernetes 动态执行池和独立注册节点；两类可同时使用。节点的执行器类型与网络标签分别展示，外部主机也可以运行自己的 Kubernetes 池。每个管理器可以承载多个独立执行环境，管理弹窗可在线调整并发、CPU、内存总额度；调低只影响后续分配。

“接入 Runner”可分别生成外部 Docker 主机或 Kubernetes 管理器的配置，并生成一次性注册令牌。外部主机的配置目录与节点身份需要持久化；生成的 Compose 只在管理器挂载 Docker socket，任务环境不挂载该 socket。GPU 设备与驱动仍需显式配置。

额外环境提示词有三层：系统配置或 Runner 页面的全局环境说明、执行设置中的环境说明、节点管理中的专属说明。内容作为字面文本附加给模型，不设置代理环境变量；全局说明也用于原有本机模式，环境和节点说明只用于实际分配的执行。运行中的执行保留已获取的说明，修改用于后续执行。地址与说明会发给所选模型，因此节点特有入口宜放在节点说明中。

完整工具镜像固定预装 vulnx 2.0.2、Nuclei 3.11.1 与 nuclei-templates 10.5.0，两种发布架构都校验官方包 SHA-256。轻量 `runner-manager` 只承担管理，工具来自每次执行所用的完整镜像。裸二进制发行包不会自动安装这些工具。

- vulnx：`vulnx search '<产品关键词>' --json`、`vulnx id <CVE编号> --json`，在线查询最新漏洞信息；需要外网，未配置 API key 可能限流。按需代理可显式传入 `--proxy`。
- Nuclei：`nuclei -tl -tags cve` 可离线列出模板，`nuclei -u <URL> -id <模板ID> -jsonl` 执行选择的模板。模板位于只读 `/opt/nuclei-templates`，每个沙箱在自己的 HOME 中初始化配置，默认禁用自动更新；需其他模板时放入工作目录并用 `-t` 指定。

`nuclei` 与 `vulnx` 以 ARTEX 原生 Skill 预置，默认对主 Agent、Planner、Worker 与自主 Agent 启用。Agent 通过 `Skill` 工具按需加载操作流程、限速、输出目录与代理使用说明；可在“系统 → Skills”按 Agent 关闭，关闭选择在重启后保留。首次安装、复用持久卷和仅更新二进制时都会补齐缺失的内置 Skill 文件，已有同名文件不会被覆盖，用户修改保留。

工具库存由实际沙箱报告，提示词只附加实际可用的工具、版本和加载 Skill 的指引。自定义镜像没有安装某项工具时，不会把它描述为可用；自定义工具版本未经验证时显示未知版本。Skill 预置不代表裸机或自定义镜像自动安装了二进制，应按 Skill 指引核对执行环境。

“系统 → LLM”可集中选择主 Agent、Planner、Worker 和目标拆解所用的模型。绑定沿用 Agent 配置并持久化，优先于任务模型链与全局默认；取消绑定恢复原有继承。正在进行的模型请求不中断，后续请求重新解析绑定。任务链不替代显式角色绑定；若另外打开绑定回退，仍按已有轮询策略处理故障。

## MCP 与依赖

现有 MCP 页面仍是统一配置入口，Agent/Skill 可见性继续生效。默认把当前线程需要的 stdio、Streamable HTTP、旧 SSE MCP 配置下发，stdio 在该线程沙箱的工作目录运行；每次执行拥有自己的连接、子进程和浏览器状态。不会复制控制服务的整个进程环境或数据库凭据。

已在本项目工具镜像内的依赖可以直接继承。新增 `npx` 或其他命令依赖镜像内工具和沙箱的网络可达性；自定义系统库、Python 包、私有二进制应准备在固定版本的派生镜像中。使用 `uvx` 的 MCP 也需要统一制作包含固定版本 uv 的环境镜像；现有默认工具镜像不提供 uv。无需逐个 runner 手工安装这些依赖。首版不从任意 shell 命令自动猜测系统安装步骤。

可显式配置：

- 中央 `mcp.<name>.location="controller"`：这个 MCP 留在控制端执行，适合仅控制端可访问的业务服务。其文件与进程也属于控制端，请只为确实需要的服务选择它。
- 中央 `mcp.<name>.override`：完整替换 runner 下发配置，名称保持不变；字段对应 `name/transport/command/args/env/url/headers/working_dir/insecure`。留在 controller 的 MCP 仍在原 MCP 页面修改，不接受这里的 override。
- 节点 `mcp_overrides.<name>`：按节点覆盖 `command/args/env/url/headers/working_dir`；适合不同网络入口或节点专用工具镜像。
- 环境 `inputs`：只读分发额外脚本/依赖目录，字段为 `name/source/target`。`source` 是控制端目录，`target` 是容器内路径；不会把这个路径当成 runner 宿主机目录直接挂载。

MCP 初始化和工具发现发生在实际执行沙箱，失败会中止该次初始化并清理已经打开的进程和连接。当前 MCP 页面的“测试/刷新”和自定义工具页面的测试仍在控制端进行；实际 runner 能力以沙箱初始化结果为准。

### 代理与流量录制

控制端的 `localhost` 代理和本机 CA 路径不能直接复制到外部节点。启用控制端代理时，为执行环境显式配置 runner 可达的 `proxy_url`；公共 CA 文件自动只读分发，Bash、WebFetch 和内置浏览器 MCP 使用该配置。如果明确不使用控制端出口/录制代理，可以配置 `direct_network=true`。这会改变该环境的网络录制能力，不能同时设置 `proxy_url`。

远程联网搜索也使用该执行环境的代理与 CA，原搜索设置中的独立代理留给本机模式。自定义 HTTP 工具若启用录制代理，会换成该环境的出口；工具中自行写死的 URL/代理则需保证在沙箱中可达。

访问内部 HTTP/SSE MCP、私有包源或镜像仓库的网络路径同样需要真实可达，无法由注册协议自动创造。节点可通过自身配置选择入口。不要在镜像中烘焙访问凭据。

## 文件一致性与恢复边界

中央持久共享存储保存正式产物。Runner 获取版本化输入并保留私有缓存，首次拉取完整快照；运行期间刷新只读范围，以便读取同任务其他线程发布的产物。自身可写目录不会被后台下载覆盖。每次工具完成、后台检查点及正常关闭时上传变化。

每个意图只写自己的 `i<id>` 目录及任务 `shared` 目录。Planner 和主对话保留原任务工作目录的写权限。技能仅分发当前 Agent 可见的目录；创建任务前上传的附件仅分发任务描述/目标中引用的 draft 上传目录。其他任务、数据库、JWT 签名密钥和任意宿主文件不在分发范围内。

所有任务线程另有只读的已发布产物目录：`<data>/tasks/.runner-published/<taskID>/`，包含 `shared/` 和各 `i<id>/`，每次工具调用前刷新。Agent 自动收到该路径说明，用它读取其他线程的新结果；写入继续使用原工作目录。原可写目录是各自的执行快照，不会被下载覆盖，即使其中的 `shared/` 已被另一线程更新。首版因此需要额外的只读文件副本与传输空间；这不是实时共享文件系统。

上传使用 SHA-256 校验、文件版本比较、临时文件和原子提交。并发修改同一文件会得到明确冲突，不能覆盖较新版本；丢失响应后的同内容重传幂等。文件 HTTP API另有短期锁令牌，过期令牌不能续作写入权限。执行租约与文件提交互斥，取消/过期的执行不能继续提交。PostgreSQL 中的控制端版本锁防止被新实例取代的旧控制端提交产物。

命令正在写文件时，后台检查点只发布已稳定的文件，变化中的文件留待下一次检查点。下载清单后源文件更新，会有限重试文件同步；不会为此重放 Bash 等工具。真正的远端版本覆盖冲突仍会停止该次执行。最终关闭会停止沙箱并严格确认文件发布，不能稳定获取最终产物时继续报告错误。诊断保留同步路径和原因，避免只显示继发的 `context canceled`。

文件网络断流、超时和 HTTP 408/429/500/502/503/504 会最多尝试 5 次，间隔指数退避并带随机抖动。上传重试始终使用同一份不可变快照和原始版本条件；即使成功回执丢失、源文件随后变化，也不会误传新内容或覆盖其他线程的新版本。权限拒绝、校验失败与真实写冲突不会按网络故障重试。每个请求默认连续 60 秒无传输进展会被取消，正常持续传输没有总时长上限；执行租约和取消仍优先生效。重试耗尽后明确报错并保留未发布缓存。

这是一套受控产物同步协议，不是跨机器 POSIX 文件系统：

- 不传播删除，避免离线或不完整扫描误删正式数据。
- 不支持分发符号链接、硬链接或特殊文件；需要的依赖应放进镜像或普通文件输入目录。
- 不承诺跨节点 `flock`、打开文件句柄、共享内存和并发原地修改的语义。
- 多文件发布不是事务。应用级快照应先生成一个完整包，再发布该包。
- 单文件默认上限 8 GiB，每个输入目录最多 100,000 个文件；大文件流式传输，暂不支持断点分片续传。
- 上传期间持续变化的文件会报告冲突/稍后重试；工具应原子生成结果或使用独立检查点文件。
- 断网或节点损坏可能丢失尚未上传的变化；节点缓存保留供核对，已成功发布的版本仍在中央。

任务事实、资产、图谱、通知与调度命令继续走中央 API/数据库，避免用目录轮询做通信。

注册令牌单次消费，节点有独立身份；每次进程启动有新 boot ID，每次控制端启动有新 epoch。活跃执行有租约，断线超过租约会停止沙箱。取消后旧结果被拒绝，容量在确认进程清理或旧租约过期后才释放。重启不承诺迁移运行中的命令、浏览器或 GPU 上下文；只能恢复已保存的对话、文件及工具支持的检查点。

后台清理对每个沙箱只允许一个正在进行的请求，失败后等待 5 秒再尝试；容器运行时恢复后可以重新确认清理，不会永久缓存第一次失败。Runner 停机保留尚在创建的会话，等待创建返回后再次清理。控制端拒绝节点身份（401/403/410）时 Runner 停止本地执行并退出，需要检查身份或重新启动注册流程。结果回执失败会退避重传，必须收到明确确认才移出待发送队列；清理成功也保留此前的原始失败原因。

无法确定是否完成的远程工具调用会产生 `execution_error`，停止当前 Agent 轮次并保留原始错误，避免自动重放可能已经发生的操作。需要核对日志和产物后决定是否续跑。

沙箱 RPC 的发送队列可以取消；正在写入的管道若阻塞，取消或 10 秒发送超时会关闭该管道并进入执行清理，避免关闭请求永久卡住。取消等待发送的请求不会打断其他正在发送的请求；已经写出但执行结果不明的命令仍按未知结果处理。

正常结束会等待最终产物同步；同步失败不会把该次 Agent 运行记为成功。持续续租的正常同步不受固定 20/30 秒限制，取消、租约失效和服务关闭仍能终止它。切换执行模式或关闭功能前，需等待远程执行与清理完成。配置损坏、数据库读取失败或失去控制端所有权时会停止执行并显示错误，不会静默使用本机。

## 测试

```sh
go test -race ./runner ./runnerfiles ./runnernode
ARTEX_TEST_PG_DSN='postgres://USER:PASS@127.0.0.1:5432/TEST_DB?sslmode=disable' go test -race ./server ./agent
(cd norma && go test -race ./agentcore ./harness ./tool ./mcp)
```

PG 测试会创建并清理专用测试数据库，不连接应用配置中的生产库。真实 Docker 集成测试位于 `integration`，需显式提供 `ARTEX_RUNNER_DOCKER_IMAGE`（含 Python/bash 的固定摘要镜像）和 `ARTEX_RUNNER_BINARY`（编译好的 Linux runner）。它验证真实容器和文件传输；没有 GPU 的测试机只能验证 GPU 分配与 Docker 参数，不能代替 GPU 驱动和实际计算验证。

额外设置 `ARTEX_RUNNER_MANAGER_IMAGE` 可验证管理器自身容器化后的 socket、路径映射和隔离。原生 Kubernetes 集成用例需在独立测试命名空间内运行编译后的 `integration.test -test.run=TestManagedRunnerEnvironments`，为测试 Pod 配置模板中的管理 ServiceAccount 与共享工作卷，并提供 `ARTEX_RUNNER_KUBE_NAMESPACE`、`ARTEX_RUNNER_KUBE_PVC`、`ARTEX_RUNNER_KUBE_WORKSPACE`、执行镜像和 runner 二进制；私有镜像可指定 `ARTEX_RUNNER_KUBE_PULL_SECRET`。测试会创建并删除执行 Pod，不应在生产池中运行。

本轮本地验证结果：

- 主模块全量 `go test -race ./...` 通过，启用了专用 PostgreSQL 与真实 Docker 集成测试。
- Norma SDK 全量常规回归通过；`agentcore/harness/tool/mcp` 关键包多轮竞态测试通过，压缩适配器并发及端到端定向竞态测试通过。
- 额外的完整 SDK `-race` 在未改动的 `noaadapter` 长会话压缩压力用例达到 10 分钟总时限，不能算完整通过；同一模块常规全量测试通过。
- 两个 Go 模块的 `go vet`、前端类型检查/缓存测试/静态构建、工作流语法检查通过。
- 最终 Dockerfile 构建、应用 API/数据库/真实浏览器/MCP 冒烟测试、镜像内 runner 启动检查通过。
- 容器化 Docker 管理器及真实 K3s 1.33.5 / containerd 的动态 Pod 池通过隔离、stdio MCP、文件回传、连续创建新环境和清理验证；原生测试使用独立命名空间及 RWX NFS，结束后无遗留执行 Pod，测试空间已清理。
- 在线降低并发、CPU、内存额度后，实际运行环境的 cgroup 配额保持不变；持久化限额、非法输入原子拒绝、并发 Pod 创建容量竞争、创建响应丢失、删除未确认、慢 MCP 初始化心跳，以及工作池扩缩容与关机竞争的回归测试通过。

CI 已包含主模块回归、Runner 与 SDK 关键模块竞态检查，以及显式开启的真实 Docker 集成测试。GPU 实际计算与跨公网吞吐仍需对应硬件和网络验证；大文件分片断点续传尚未实现。


## 共享存储执行（Kubernetes）

长任务建议将执行环境设为 `file_mode: "shared"`。启动仅检查挂载路径和文件系统剩余容量，准备成本随挂载数量增长，不再扫描、哈希、复制任务历史。已有任务目录、会话和进度保持原位置；`published` 是同一任务目录的只读别名，不产生第二份数据。原有快照模式保留给外部 Runner，准备上限为全部挂载合计 10,000 个文件、1 GiB，每次目录遍历至多 20,000 个条目、64 层目录；超限明确失败，不截断输入后继续运行。

每次执行仍创建独立 Pod，保留只读根文件系统、无特权运行、独立 HOME/tmp、CPU/内存额度和租约 watchdog。HOME 256 MiB、tmp 512 MiB、未另行挂载的 `/workspace` 512 MiB，Pod 临时存储上限 1 GiB。任务产物是持久共享目录，Worker 仅能写自己的 `i<id>/` 与 `shared/`；主对话与 Planner 保持原任务范围写权限。共享文件实时可见，没有快照 CAS；协作产物请用唯一名字，写同目录临时文件后原子重命名，不同时覆盖一个文件。租约失效会停止进程，但已经写入的数据不会回滚。

管理员在 Runner 的 `kubernetes` 下配置映射，例如：

```json
{
  "shared_mounts": [
    {"source":"/app/data/tasks","path":"/var/lib/artex-shared/tasks","pvc":"artex-shared-tasks","sub_path":""},
    {"source":"/app/data/sessions","path":"/var/lib/artex-shared/sessions","pvc":"artex-shared-sessions","sub_path":""},
    {"source":"/app/data/skills","path":"/var/lib/artex-shared/skills","pvc":"artex-shared-skills","read_only":true},
    {"source":"/app/data/drafts","path":"/var/lib/artex-shared/drafts","pvc":"artex-shared-drafts","read_only":true},
    {"source":"/app/data/runner-inputs","path":"/var/lib/artex-shared/runner-inputs","pvc":"artex-shared-inputs","read_only":true},
    {"source":"/app/data/.resource-library/versions","path":"/var/lib/artex-shared/resources","pvc":"artex-shared-resources","read_only":true}
  ],
  "shared_min_free_bytes": 1073741824,
  "shared_min_free_inodes": 10000
}
```

可用 `kubectl get pv <controller-pv> -o json | python3 ci/shared-storage-manifest.py artex-runners` 生成指向已有 NFS 子目录的静态 CSI 别名，审阅后 apply；脚本仅处理存储元数据，要求集群已安装 `nfs.csi.k8s.io`，无需每个节点另装 NFS 挂载程序。

每个映射的 `path` 必须是管理器挂载的 PVC/sub_path，且与 controller 的 `source` 指向相同数据。PVC 必须存在于执行 Pod 的命名空间；跨命名空间的 NFS 部署可为指定子目录创建静态 PV/PVC 别名，使用 `Retain`，不要创建新目录冒充已有数据。管理器仅挂载所需数据子树；数据库、JWT、配置凭据及整块 controller 数据根目录不应暴露。额外 inputs 也必须有映射；缺失映射、路径穿越、符号链接挂载、只读越权均会失败。共享能力由新管理器自动上报，旧节点不会被选择；共享文件 API 拒绝快照读写，避免旧节点静默退化。

共享 Pod 不配置 `fsGroup`，防止启动时递归修改大卷权限。迁移时一次性为上述目录配置管理器/执行器的 UID/GID（模板为 10001），并确保控制器未来新建的目录也可由该身份访问；不要每次启动递归 chown。映射检查不遍历任务文件。

共享存储默认在接入和每次后台检查时保留至少 1 GiB 可用空间、10,000 个 inode，不足会拒绝新执行或停止当前执行并保留持久文件；不报告 inode 总量的文件系统只检查字节。这是容量保护而非每任务硬配额，检查周期内仍可能消耗余量。NFS 的 PVC 请求容量不是硬配额；严格持久容量/文件数上限需在存储端设置，不通过每次扫描任务实现。

迁移顺序：备份数据库与部署配置；停止接纳新执行并让旧快照执行完成清理，保留未发布缓存；原地配置共享卷别名和权限；升级管理器/控制器后，在无保留租约时切换环境模式；验证合成任务后恢复原有任务进度。不要把未知结果的工具当成未执行而自动重放。切换接口会拒绝仍占用租约的变更，不同环境也不能同时对重叠目录混用共享和快照。回滚须同样先清理共享执行，再切回快照；大目录可能触发快照上限，不能依靠回滚恢复全量同步。
