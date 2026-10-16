---
name: python-environment
description: 查询 ARTEX 默认沙盒的 Python 预装包、导入名和版本，选择现有依赖编写 Python 脚本，处理代理、离线解析和依赖兼容问题。适用于确认 Python 环境能力或排查导入失败。
license: AGPL-3.0-only
compatibility: ARTEX 默认 Linux amd64/arm64 完整镜像，Python 3.12；外部 Runner、自定义镜像和旧版本以实际 Python 环境为准。
---

# Python 预装环境

默认解释器为 `python3`，下列库已随镜像安装，可直接导入。优先使用预装依赖；具体版本固定在 `/opt/artex-tools/requirements.lock`，当前解释器中的实际版本可用 `python3 -m pip list` 或 `python3 -m pip show 包名` 查询。

## 预装包与导入名

表中省略了部分传递依赖；未标箭头的包通常使用同名导入。

| 用途 | 包与常用导入名 |
| --- | --- |
| HTTP、SOCKS、异步请求 | `requests[socks]` → `requests`；`PySocks` → `socks`；`httpx[socks,http2]` → `httpx`；`aiohttp`；`aiohttp-socks` → `aiohttp_socks`；`python-socks` → `python_socks`；`requests-toolbelt` → `requests_toolbelt` |
| SSH／SFTP | `paramiko`、`asyncssh` |
| WebSocket | `websockets`；`websocket-client` → `websocket` |
| gRPC／protobuf | `grpcio` → `grpc`；`protobuf` → `google.protobuf`；`grpcio-reflection` → `grpc_reflection`；`grpcio-tools` → `grpc_tools` |
| DNS、地址与域名 | `dnspython` → `dns`；`netaddr`、`tldextract`；标准库 `ipaddress` |
| HTML／XML／YAML | `beautifulsoup4` → `bs4`；`lxml`（常用 `lxml.etree`）；`defusedxml`；`PyYAML` → `yaml` |
| 密码学、JWT、LDAP | `cryptography`；`pycryptodome` → `Crypto`；`PyJWT[crypto]` → `jwt`；`ldap3` |
| 报文与 PCAP | `scapy` → `scapy.all`；`dpkt` |
| MQTT、串口、Modbus | `paho-mqtt` → `paho.mqtt.client`；`pyserial` → `serial`；`pymodbus` |
| 二进制编码与结构 | `construct`、`msgpack`、`cbor2` |
| ELF／PE、反汇编、模拟与符号执行 | `pyelftools` → `elftools`；`pefile`、`capstone`、`unicorn`；`pwntools` → `pwn`／`pwnlib`；`angr`、`claripy`；`z3-solver` → `z3` |
| 数据库客户端 | `PyMySQL` → `pymysql`；`psycopg[binary]` → `psycopg`；`redis`、`pymongo`；标准库 `sqlite3` |
| JSON 查询与校验 | `jmespath`；`jsonpath-ng` → `jsonpath_ng`；`jsonschema` |
| 脚本辅助 | `tenacity`、`psutil`、`rich`、`tqdm`、`packaging` |
| 离线 IP 数据读取 | `maxminddb`；`py-ip2region` → `ip2region` |

## 使用要点

- 包名与导入名可能不同，例如安装名 `PySocks` 对应 `import socks`。WebSocket 客户端是 `websocket-client`，无需再装名称相似的 `websocket` 包。
- requests 的 `socks5h://` 代理在代理端解析域名；HTTPX 使用 `httpx.Client(proxy=proxy_url, http2=True, timeout=20)`，其 SOCKS／HTTP2 依赖已安装。
- aiohttp 的 SOCKS 支持来自 `aiohttp_socks.ProxyConnector.from_url(proxy_url, rdns=True)`，在异步函数中创建并传给 `ClientSession(connector=connector)`；使用环境变量中的 HTTP 代理需设置 `trust_env=True`。单独安装 PySocks 不会自动为 aiohttp 配置代理。
- protobuf 代码生成可用 `python3 -m grpc_tools.protoc`，无需临时安装 Python 编译插件。
- 当前锁定 angr 9.3 系列，使用顶层 `import claripy`；Z3／Unicorn 与 angr／pwntools 已配套验证，不单独升级这些全局依赖。大型分析库按需导入。
- 数据库包提供客户端，不附带数据库服务。

`tldextract` 自带公共后缀快照。离线使用时关闭联网更新和磁盘缓存：

```python
import tldextract

extract = tldextract.TLDExtract(suffix_list_urls=(), cache_dir=None)
parts = extract("https://service.example.co.jp/path")
print(parts.top_domain_under_public_suffix)  # example.co.jp
```

## 导入失败与额外依赖

先用 `python3 -c 'import sys; print(sys.executable)'` 确认解释器，再用同一解释器的 `python3 -m pip show 包名` 检查安装情况；检查工作目录是否存在遮蔽库的同名文件，例如 `requests.py`、`socket.py`。外部 Runner 或旧镜像可能不包含上述预装集合。

需要额外依赖或不同版本时，在本次工作目录创建独立 venv。普通 venv 不继承预装包；需要复用时使用 `python3 -m venv --system-site-packages .venv`，需要完全独立的版本组合时省略该选项。
