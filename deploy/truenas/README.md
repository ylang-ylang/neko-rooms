# TrueNAS：发布与原生单 TCP 入口

## 日常发布：一条命令，保留现网拓扑

本目录属于 **neko-rooms 源码仓库**；独立的 **truenas-apps** 仓库只接收根目录 App YAML，不放脚本、模板、测试或源码。本目录不保存第二份 `neko.compose.yaml`。

从 **neko-rooms 仓库根目录**执行：

```sh
bash deploy/truenas/publish-to-tn.sh
```

它薄调用本源码仓库现成的 `python3 scripts/release.py publish`（增量构建/缓存/按组件发布），传递 `release.json`，再通过既有 SSH key 调用 TN 的 `update.py --release ...`。TN 只升级当前 App 的 Manager image，Browser 镜像仅预拉备用；不改当前网络/端口/env/卷/镜像 allowlist，不 stop/recreate 持久房。没有重复媒体、CDP、clone 或认证回归。原配置私备，官方 `app.update` 失败则由同一 API 恢复。

本次已交付的 Manager 为下文 `97ffae9` digest。**当前 TN 仍保持原 direct 拓扑，入口 `http://192.168.202.11:28322/manage/`，Owner 使用原 `rooms.env`。** 首次单口拓扑迁移与日常镜像更新分开：NAS 实际只有 `.202.11`，不能将解析到另一主机 `.201.11` 的 `truenas.lan` 用来切换。本页后续单口配置待有正确指向 `.202.11` 的 DNS 名再执行，不是每次发布步骤。

仅更新已发布 Manager（在 TN 本机）：`sudo -n python3 /home/truenas_admin/neko-native-release/update.py --manager-image docker.io/ylangylang/neko-rooms:single-port`。

## 首次原生单口部署（需正确 DNS）

唯一部署清单源仍是独立 **truenas-apps 仓库根 [`neko.compose.yaml`](https://github.com/ylang-ylang/truenas-apps/blob/main/neko.compose.yaml)**，不是另建 App，也不迁到 `neko/` 子目录。实现直接取自 `neko-rooms@97ffae9ed0acffd698a112f00d0167112441f795` 的 `examples/single-port-relay/`；三个代理模板原样保留。原生 Neko 3.2 UI、核心和 Chromium 不替换为 aggregate/MSE 页面。

- **28322/tcp** 从旧 Manager HTTP 入口升级为同端口 **HTTPS/WSS + TURN/TLS + Agent CDP**。Nginx 保留 HTTP ALPN/SNI 快路径，无 SNI/ALPN 时由 HAProxy 在 TLS 解密后区分 HTTP/STUN；普通 Playwright 不需特殊传输参数。
- Manager、HAProxy、coturn、所有新建 relay 房间不发布额外 HostPort；TURN UDP 分配池只在现有 `neko-rooms-net` 内。普通 HTTP 反代无法代替这个入口。
- 旧默认房 `28320/28321`、`52100-52110/udp`、`profile/`、`downloads/`、`config/`、认证文件原位保留。不要将这些旧端口当新公网入口。
- `rooms-data/rooms/NAME/{profile,downloads}` 不迁移；旧测试数据也不删除。房间名字是存储身份，容器 ID 可变。

## 1. 准备一次，TN 直接拉镜像

在 TN 上分别准备 `neko-rooms` 与 `truenas-apps` 两个独立 checkout，以下命令从 **neko-rooms 根目录**执行。Python 3、OpenSSL 和 TN 自带 Compose **2.32.3** 足够；不需安装 Python 包或本地构建镜像。默认读取相邻 `../truenas-apps/neko.compose.yaml`；不相邻时显式设置清单路径，不能把脚本复制回清单仓库。

```sh
app=/mnt/ylangsPool/appdata/neko
manifest=../truenas-apps/neko.compose.yaml  # 不相邻时改为实际绝对路径
test -f "$manifest"
test -e "$app/native-relay.settings" || install -m 600 deploy/truenas/settings.example "$app/native-relay.settings"
docker network inspect neko-rooms-net --format '{{json .IPAM.Config}}'
```

在私有 `native-relay.settings` 填写：

1. `APP_HOST`：客户端实际能解析、到达此 TN/入口的 DNS 名。`TURN_HOST` 留空即可共用同一 FQDN；Firefox TURN/TLS 拒绝 IP 字面量。不要默认 nip/sslip 可解析，也不要自动改 hosts。
2. `PUBLIC_BIND_IP`：默认沿用已授权 NAS 地址 `192.168.202.11`；若明确要多个接口监听，可显式改为 `0.0.0.0`。DNS 可以指向 NAS 另一真实接口或已授权 TCP 转发入口，**不要求 DNS IP 字面上等于绑定 IP，也不因此假定它们同机**。目前工作站 `truenas.lan`/`truenas` 解析到 `192.168.201.11`，尚无同机证据，不能拿它冒充已验证的 `.202.11`。
3. `PUBLIC_PORT=28322` 默认不换旧外部端口；HTTPS 和 `turns:` 必须用这同一个端口。若以后 WAN 转发，需用户授权后配置原始 TCP/L4 透传到该端口，不能经仅 HTTPS 的反代/CDN。
4. `ROOM_NETWORK_CIDR`：上面实际 bridge 子网；`TURN_PRIVATE_IP`：其中明确未占用的静态地址，不能是 gateway。脚本只检查该现有网络，不创建/重置网络。
5. 已有公信证书：填写 NAS 上实际 `TLS_CERT_FILE` fullchain 和 `TLS_KEY_FILE`，证书须覆盖两个配置名；密钥权限 0600。脚本不覆盖它们。

已有证书执行：

```sh
python3 deploy/truenas/configure.py --compose-template "$manifest" --settings "$app/native-relay.settings" --public
```

**仅 LAN 私有 CA 模式**：保持 TLS 两字段空白，显式加 `--dev-ca`。仍需客户端可解析的真实 DNS 名。脚本生成/复用 `native-relay/tls/`，不导入信任、不改 DNS，也不替换已存在的 CA/key。客户端需明确导入 `native-relay/public/ca.crt`；它是唯一可经同端口 `/ca.crt` 下载的公共证书，不包含私钥。叶证书有效 30 天，到期/更换 DNS 时在原命令显式加 `--renew-server-cert`，只换叶证书，保留 CA、keys、TURN 凭证。

脚本产出 **`/mnt/ylangsPool/appdata/neko/native-relay/neko.compose.yaml`**。将其全文粘贴到现有 TN **neko** App 的 YAML 编辑器并由操作者保存更新。**不要直接粘贴尚含 `${...}` 的仓库源文件**；TN 编辑器不会读取 checkout 的 `.env`。生成 YAML 保留扁平布局、只引用 NAS 绝对路径，不依赖本机 cache；原 `rooms.env/neko.env` 仍由 TN Compose 按原 dotenv 语义加载，TURN 的 `relay.env` 用 Compose 2.30+ 支持的 `format: raw`。输出不展开永久密码，也不启动/停止任何服务。

根 YAML 的无网络 `room-image` 一次性服务只执行 `/bin/true`，先拉取与 Manager allowlist **完全相同**的房间镜像，不运行浏览器、不挂数据。Manager 在它成功后启动。新原生栈无需额外手动 build/load，不需要把镜像搬到本地标签。旧默认 `neko/cdp-proxy` 保留原镜像及 `pull_policy: never`，继续使用该既有 App 的已加载镜像，不借升级重做默认房。

## 2. 旧房必须显式迁移，不是换 Manager 就结束

旧 `http/direct` 房间的实际网络标签、容器、端口不会因 YAML 更新自动变化；`http` 房仍会走旧自定义聚合器，直接点默认 Recreate 还可能因旧镜像不在新 allowlist 被拒绝。

先通过新 `https://APP_HOST:28322/manage/` 用**原 `rooms.env` Owner 用户名/密码**登录；Manager 重启后旧 session/临时 Agent key 失效，重新登录/签发即可。**不要批量删除旧房，不要修改默认 TN-owned neko 服务。**

每次只选一个要迁移的房间：

1. 暂停并确认 Agent 没有在途 CDP 操作；停止该房以落盘，备份其 `rooms-data/rooms/NAME/`（包括 SQLite WAL）。暂停房先解除 pause 再正常停止；不要直接复制仍在写入的 profile。保留其运行/暂停状态供迁移后手动恢复。
2. 保留 `GET /manage/api/rooms/ID/settings` 的私有备份：含该房密码，不能贴进 issue/日志。确认 profile/downloads 的实际挂载仍为 `/profile`→`/home/neko/profile`、`/downloads`→`/home/neko/Downloads`，不要把整个 `/etc/neko` 或自定义 `neko.yaml` 覆盖 Manager 的原生 Neko-3 bridge。
3. 在**已登录 Owner 的同源浏览器控制台**执行下面的显式请求；只填写所选 NAME，新镜像默认与发布通道一致。若已在设置锁版本，`image` 也须填相同 `ROOM_IMAGE`。它只覆盖 API/image，其余名字、密码、挂载、资源、策略保持原 settings；`start=false` 避免自动开房。服务先验证再移除旧容器，但后续启动失败仍需用私有 settings 备份恢复，故先备份。

```js
const name = '填写所选原房名';
const image = 'docker.io/ylangylang/neko-chromium-cdp:single-port';
const room = await fetch(`/manage/api/rooms/${encodeURIComponent(name)}/by-name`).then(r => {
  if (!r.ok) throw new Error(`lookup ${r.status}`); return r.json();
});
const result = await fetch(`/manage/api/rooms/${room.id}/recreate?start=false`, {
  method: 'POST', headers: {'Content-Type': 'application/json'},
  body: JSON.stringify({api_version: 3, neko_image: image})
});
if (!result.ok) throw new Error(`recreate ${result.status}`);
console.log('所选房已重建；请在 Owner 页手动启动并验收');
```

4. 从 Owner 页启动该房；`/room/NAME/` 必须显示原生 Neko UI，用**原房间密码**登录，检查音视频/输入/剪贴板和原 cookies/downloads。Docker 检查该房 `HostConfig.PortBindings` 为空，网络模式标签为 `relay`。按需恢复暂停状态，其余旧房不动。

新建房也用 API **3** 和 allowlist 里的新镜像、上述 Profile/Downloads 私有挂载。Neko 3.2 的 Viper 忽略空 env，故必须保留 Manager 原有 native-YAML startup bridge 来禁用公网 IP 探测；本方案不是只把 `NAT1TO1` 清空。Owner 的 Clone 只填新名字，使用其现有独立复制实现。

Agent 从 Owner 页生成临时 room-bound Bearer key，用同一 HTTPS 端口 `/rooms/NAME/` 的 discovery/WSS。Owner cookie、原生房密码都不是 CDP key。连接现有浏览器/context/tabs；切回人控前暂停 Agent，Neko 控制锁不阻止 CDP，不要发 `Browser.close`。

## 3. 通道升级、版本锁和验证

默认 `MANAGER_IMAGE=docker.io/ylangylang/neko-rooms:single-port`、`ROOM_IMAGE=docker.io/ylangylang/neko-chromium-cdp:single-port`，`RELEASE_PULL_POLICY=always`。以后发布更新通道，**操作者在 TN 选择更新/重新部署**即可拉取；CI 不登录 NAS，不自动重启 App，也不自动重建持久房。

需要锁版本/回滚：在原 `native-relay.settings` 改这两个 image 为发布摘要的 immutable tag 或 `repo@sha256:...`，重跑同一 configure，再更新原 App YAML。已有房要换 image 仍须逐房显式 recreate；仅切 tag 不会更换正在运行的浏览器。离线存档仅在已加载全部所需镜像后显式设 `RELEASE_PULL_POLICY=never`，并将 image 指向真实已加载的对应版本；不要把未发布的 local tag 当公共镜像。

本次两个 linux/amd64 镜像均已通过匿名 manifest/config 读取及实际按 digest pull，无需 TN registry 登录。可将下面两行直接替换设置里的通道值来锁定本次验收版本：

```text
MANAGER_IMAGE=docker.io/ylangylang/neko-rooms@sha256:c8c2f4d8aba0228ce33a28d4d0a0d692439a9550bec6c8f17cb3bc1da73db592
ROOM_IMAGE=docker.io/ylangylang/neko-chromium-cdp@sha256:96e413b7a82a0ec6043597151ce5f2183d34188aa81cc692af3365eafa1bd4e2
```

相同版本的 immutable tags 分别为 `neko-rooms:single-port-97ffae9`、`neko-chromium-cdp:3.2.0-1`；两个默认 `:single-port` 通道在此次发布时也指向上述 digest。发布验收依据为镜像发布任务的匿名 pull 记录，不是根据标签存在推测成功。

以上是镜像发布与 YAML 准备，不等于已有 NAS App 更新或 WAN 开通。DNS、证书信任和入口的实际可达性必须从使用客户端验证。部署后运行只读 probe（私有 CA 模式附加 `--dev-ca`）：

```sh
python3 deploy/truenas/configure.py --settings "$app/native-relay.settings" --check-endpoint
```

它报告所选 DNS 的实际 IP，并验证同端口 TCP、两个名字的 TLS 信任/SAN/有效期和 Owner form 200/无 Basic challenge；不把 DNS 与某接口 IP 强行比较，也不证明两个接口属于同一 NAS。若从工作站检查，可带 `--app-dir` 指向本机持有的私有 CA 公共证书目录，配置与信任文件须私下传递。它不改文件、不登录、不做 TURN allocation，原生媒体/CDP 仍需上一节房间级验收。不在部署前以尚未监听的 TLS 新端口作为 render 前置。

本地针对性测试（从 neko-rooms 根执行，仅临时目录，不 pull/build/start 容器）：

```sh
PYTHONDONTWRITEBYTECODE=1 python3 -m unittest discover -s deploy/truenas -p 'test_*.py' -v
# 清单 checkout 不相邻时：TEST_COMPOSE_TEMPLATE=/path/to/truenas-apps/neko.compose.yaml。
# 如有 NAS 同版独立二进制：TEST_COMPOSE_BINARY=/path/to/docker-compose-v2.32.3 加在上面命令前。
```
