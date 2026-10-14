# Standalone execution node

`artex-runner` runs on a Linux Docker host or manages native Kubernetes Pods. The controller keeps agent reasoning, model credentials, permissions and product state. Each leased agent attempt gets a fresh container or Pod for its execution; a physical node or managed pool can host multiple attempts according to its advertised capacity. The node only makes outbound requests to the controller.

See [the deployment guide](../docs/experimental-runners.md#容器化管理器与动态执行池) for the optional Compose manager and Kubernetes pool templates. One manager creates execution environments on demand. The UI can persistently adjust concurrency, CPU and memory budgets within the node's hard capacity; reductions affect subsequent allocations while existing executions retain their resources. The Kubernetes backend uses shared RWX storage, scoped Pod permissions, no host socket and no execution service account token. GPU execution currently uses the Docker backend.

Optional `warm_idle` on a controller environment prewarms concrete queued Kubernetes layouts, within the same physical Pod/CPU/memory budgets. Image, environment, resources, working directory and file mounts must match exactly. Inputs and MCP initialization start only after exclusive assignment. Every Pod is used once and destroyed. When no matching Pod is ready, the unchanged cold Pod can start alongside input transfer. Idle cleanup never removes possible recovery artifacts; the Docker backend is unchanged. See the deployment guide for configuration and retention details.

Build with `go build -o artex-runner ./cmd/artex-runner`. A minimal node configuration is:

```json
{
  "server_url": "https://artex.example.org",
  "name": "worker-01",
  "data_dir": "/var/lib/artex-runner",
  "capabilities": {
    "slots": 4,
    "cpu_millis": 4000,
    "memory_mib": 8192,
    "environments": ["default"]
  }
}
```

Run `artex-runner --config runner.json --token-file enrollment-token`. The enrollment token is only needed once. `ARTEX_RUNNER_ENROLLMENT_TOKEN` is also supported and is removed from the runner process environment before execution starts. The persistent credential is saved atomically as `credentials.json` with mode `0600`; the data directory is `0700`. Only one node process can own that directory. Restarting creates a new boot identity and removes containers from the preceding boot before accepting work.

The controller specifies an image by immutable `repository@sha256:digest`; the node pulls it automatically. The image must contain `/usr/local/bin/artex-runner`, the requested interpreters, MCP launchers and their locked dependencies. `allowed_images` can restrict accepted digests; `image_override` can select a node-specific pinned image. Changing these does not edit central MCP configuration. Build/publish a new environment image when dependencies change, then select that version centrally. Arbitrary command strings cannot reliably reveal which OS packages, binaries, browser assets or local files they require; missing dependencies fail explicitly inside the container.

MCP connections are initialized once per session and reused by its calls. `mcp_overrides`, keyed by MCP name or ID, can replace `command`, `args`, `env`, `url`, `headers` and `working_dir`. Controller loopback addresses and untransferred absolute paths require a deliberate reachable override or an updated runtime image. Custom command/Python/HTTP tools execute inside the same sandbox. The process receives only explicit tool/MCP configuration and image environment; controller database and model credentials are not inherited.

Docker uses a read-only root filesystem, independent HOME/tmp, a separate process namespace, dropped capabilities, no-new-privileges, PID limits, CPU/memory limits and explicitly assigned GPU devices. No Docker socket, host PID namespace or host network is mounted. Default per-session limits are one CPU, 1 GiB RAM and 256 PIDs. GPU assignments are exclusive whole devices; GPU memory is a scheduling constraint, not an enforced memory partition. The daemon runs containers under the node process's UID/GID so private file caches remain writable.

The default `snapshot` file mode transfers a versioned snapshot. Preparation is capped at 10,000 files and 1 GiB across all mounts; each traversal also caps directory entries and depth. Exceeding a limit fails explicitly before any blob download. The node downloads into its private cache and mounts the declared logical target paths; controller `Source` paths are never interpreted as node host paths. Shared inputs are read-only and refreshed before calls. Only the session's declared writable paths have writable mounts. New/changed writable files publish after each call, periodically during background activity, and on normal close, using compare-and-swap digests. Concurrent changes fail explicitly without overwriting the other version. On conflict, local files remain under the private session cache for recovery, the execution fails, and side-effecting calls must not be replayed automatically. Deletions are not propagated. Symlinks and special files are rejected. Large files stream without the protocol polling timeout, but currently do not have chunk-level resume.

Each tool call has a stable ID. Duplicate commands/results are replayed from their recorded outcome, not executed again; conflicting reuse of an ID is rejected. A local watchdog stops the container when the controller stops renewing its lease, even while an image pull, tool call or file transfer is in progress. Cancellation stops immediately without publishing more files. A normal close first stops sandbox processes and then publishes the final snapshot. Failure to prove process termination is reported as an unknown outcome; capacity is not silently released.

`unsafe_process: true` is only for explicit development, with no OS isolation, no transferred mount support and no GPU allocation. It must not be used for untrusted agents. HTTP also requires explicit `allow_insecure_http: true`; production HTTPS certificate verification is enabled by default.

Tests: `go test -race ./runnernode ./mcphttp`, plus `(cd norma && go test -race ./mcp)`. The repository integration test exercises real Docker containers, file publication and MCP execution.

Kubernetes environments can set `file_mode: "shared"` to directly mount administrator-provisioned `kubernetes.shared_mounts` mappings. Session startup checks only the declared path components and filesystem capacity; it does not enumerate, hash, copy or periodically publish task history. `workspace` and the read-only `published` alias point to the same task subtree. Other tasks and controller credentials remain outside the sandbox. HOME/tmp/scratch stay disposable, while declared task outputs persist in place. Shared writes are live POSIX writes, without snapshot compare-and-swap: use unique output names and same-directory temporary-file + rename publication. Concurrent overlapping snapshot/shared execution is refused. Switching file mode requires an idle broker.

Shared mappings specify `source` (controller prefix), `path` (manager mount), `pvc`, `sub_path`, and optional `read_only`. Unmapped sources, symlinked mount paths and writable requests under read-only mappings fail closed. No snapshot fallback is allowed. Shared execution Pods omit `fsGroup` to avoid recursive ownership work at every startup; provision compatible ownership once. A default free-space reserve of 1 GiB and 10,000 inodes is checked at admission and every background checkpoint, without walking files; node settings `shared_min_free_bytes` and `shared_min_free_inodes` can increase it. Filesystems reporting no inode capacity skip the inode check. This is a stop/admission guard, not a hard per-task filesystem quota. Configure durable byte/inode quotas at the storage backend when required; an NFS PVC capacity request alone does not enforce them.
