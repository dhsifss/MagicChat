# 手机推送接入与运维

移动端只调用当前登录的 MagicChat Server。Server 使用推送后台签发的长期凭据换取短期访问 Token，再调用 `/v1/endpoints` 和 `/v1/messages`；认证客户端并发复用并按到期时间刷新 Token。推送服务的凭据保留在 Server。

## 配置

```dotenv
PUSH_SERVICE_URL=https://push.example.com
PUSH_SERVICE_CREDENTIAL=mpt_<record-id>.<secret>
PUSH_APNS_DEVELOPMENT_CHANNEL_ID=<32位小写hex通道ID>
PUSH_APNS_PRODUCTION_CHANNEL_ID=<32位小写hex通道ID>
PUSH_FCM_CHANNEL_ID=<32位小写hex通道ID>
PUSH_HUAWEI_CHANNEL_ID=<32位小写hex通道ID>
PUSH_XIAOMI_CHANNEL_ID=<32位小写hex通道ID>
PUSH_OPPO_CHANNEL_ID=<32位小写hex通道ID>
PUSH_VIVO_CHANNEL_ID=<32位小写hex通道ID>
PUSH_HONOR_CHANNEL_ID=<32位小写hex通道ID>
PUSH_MEIZU_CHANNEL_ID=<32位小写hex通道ID>
```

URL 和凭据同时留空关闭推送；启用时至少配置一个通道。通道须属于该凭据的业务、完成发布和准入，并与应用包名、APNs 环境或 Firebase 项目匹配。凭据需 `endpoints:read`、`endpoints:write` 和 `messages:send` 权限，以及 transaction 类别、normal 优先级的发送授权。HTTP 仅用于受保护的内部网络；HTTPS 正常验证服务端证书。

首次启用会自动创建 `data/push/credential.key`，Compose 对应 `./data/server/push/credential.key`。备份及恢复必须保留该文件和数据库。内部加密密钥用于设备端点引用及通知路由的持久化加密，独立于服务凭据轮换。认证客户端来源为 magic-admin 的 `sdk/go/pushclient`，在 `server/internal/infrastructure/pushclient` 内随本服务独立构建。

移动端按构建配置接入华为 HMS、小米、OPPO、vivo、荣耀、魅族原生 SDK，同时支持 FCM。配置及版本见 [原生 SDK 接入](mobile-vendor-push.md)。iOS 使用 APNs Token 与签名对应的推送环境。需要重新构建并发布移动端，与 Server 配套升级。

## 设备快照、端点与自动选路

客户端向当前认证的 Server 提交随机安装 ID、随机账号绑定 ID、平台、制造商、型号及 `registrations` 全量快照。每项记录真实注册成功的 provider 和 Token；APNs 额外携带签名的 development/production 环境。安卓厂商信息与 Token 注册分开记录，品牌不等于注册成功，一个安装可同时取得多个厂商 Token。

Server 在取得有效 Token 后调用 `/v1/devices` 上报设备与 `registered_providers`，读取该设备的端点目录，对未过期的已有端点通过 PUT 与当前版本更新 Token/到期时间，仅为尚未登记的服务创建端点。端点过期或上游绑定已关闭时，先注销旧端点并使用新的上游安装/绑定引用；保留当前本地绑定 ID，使旧通知的点击校验继续有效。所有端点使用同一个安装引用及账号绑定引用。Token 暂时为空时直接返回临时不可用，不调用上游设备上报，也不清空已生效的路由。客户端按原生构建中的厂商 SDK 与 Firebase 配置识别支持的通道；构建包含的 SDK 不都要求在当前设备取得 Token。对已成功注册过的通道，查询失败或超时时保留其最近成功取得的 Token 与本地绑定，提交完整缓存快照并安排重试；SDK 明确判定设备不支持时，移除该通道的缓存。首次登记只要成功取得并被 Server 接受的通道可用，即完成登记并启用远程推送，其他通道仍通过正常刷新及 Token 回调继续发现。构建中已移除的通道从快照中移除，Token 更新与集合调整保留本地绑定 ID。配置的通道用于登记对应 SDK 的 Token，不决定发送优先级。

Server 加密保存上游设备/安装 ID 和端点 ID 集合。发送调用 `/v1/messages` 的 `installation_ids`，由推送服务结合制造商、平台、已注册服务、当前绑定和有效端点选择厂商；已注册原厂通道优先，FCM 位于后面，降级沿用推送服务的状态、TTL 和额度规则。通知正文固定为“你收到一条新消息”，附带随机绑定和路由引用；点击后由当前登录账号解析。

数据库原有 `gateway_grant_id` / `send_token_ciphertext` 列继续承载本地绑定 ID / 加密上游绑定快照，不重写历史迁移。同一安装的完整登记操作由独立 PostgreSQL session advisory lock 串行执行，覆盖准备、暂停提交与完成之间的事务间隙，避免重叠 provider 更新混用设备快照和端点计划。锁和各阶段数据库操作复用同一条专用连接，避免外层事务占满池后再等待内层连接；各阶段仍独立提交恢复信息。释放锁使用独立取消上下文，无法确认释放时丢弃物理连接。登记分为两个本地事务：先保存含上游设备 ID、安装引用、绑定引用及每个待创建端点的完整请求/幂等键的加密 `registering` 记录，再创建/更新端点并置为 `active`。注销未确认的登记时，先以原键和原内容重放 POST，协调仍在上游执行的请求，取得端点 ID 后再注销；重放未确认时保留清理记录。空端点快照不能单独作为未完成登记的清理成功依据。Token 在保存登记计划前校验长度、UTF-8、首尾空白、控制字符及不可见格式字符；首次创建收到明确准入或 Token 校验拒绝（如 `channel_not_ready`、`invalid_endpoint_token`）时，将拒绝结果提交到加密登记计划；换通道、注销和退出清理跳过已确认拒绝的请求，同通道恢复使用新的登记幂等键。此前已存在结果不明请求时，后续拒绝不能证明早先请求不会晚提交，仍保留其清理信息。中断、响应丢失或第二次本地事务失败后，注销/退出/账号切换沿用这份记录清理，不依赖客户端知道登记是否成功。首次同步按新版本重新登记。每个账号绑定使用独立上游安装引用；账号切换时建立新绑定并撤销旧端点。通道配置变更或端点过期时轮换上游绑定，同时保留本地绑定 ID。轮换检测与上游注销分开提交：先在本地持久化 `registering` 和旧绑定的清理引用，再重新取得登记锁并注销旧端点、上报新设备。后续设备上报或本地事务失败不会回滚已提交的暂停状态，通知继续等待登记恢复。发送与登记共用安装锁，发送取得锁后重新读取任务租约及授权状态，并持锁到上游响应处理完成；领取任务时缓存的 active 状态不能作为发送依据。首次提交目标通过同一连接中的独立数据库操作先提交，再调用推送服务，继续支持响应丢失后的幂等恢复。全部撤销通过 Server 完成，网络失败保留待清理记录。迁移 `00047_add_push_endpoint_revocations` 在删除本地绑定时，同事务保存加密端点引用到独立清理队列；会话/账号级联删除也执行该触发器。队列不依赖原会话或账号，沿用原加密关联 ID 解密。后台 Worker 在独立循环中按 `next_attempt_at` 处理上游注销，慢清理不阻塞消息展开或发送，每个失败任务保存尝试次数并以 30 秒起步指数退避，最长约 8.5 小时；失败记录保留，暂时离开到期集合，使后续安装的清理继续推进。重新登录登记前会忽略退避时间，先清理该安装的历史任务。成功登出表示会话已撤销、清理任务已持久化，不代表上游端点已全部注销。

同账号、同设备的例行续期保留绑定 ID，先保存新的绝对到期时间，再同步 Server；Server 使用端点目录中的当前版本执行 PUT，失败重试重新读取版本。更新已提交而响应丢失时，不创建重复端点。续期后的通知点击校验仍能识别此前已显示的通知。

远端成功、本地事务失败时，重试读取同安装的上游端点目录并更新已有端点；尚未取得 ID 的创建请求继续由已持久化的登记计划协调。准备登记按新上报 provider、当前配置通道、端点 active 状态及到期时间判断可用性，历史端点 ID 仅保留作清理引用。只替换原有唯一 provider 时保持 `registering`；新列表仍包含可用旧通道时允许继续发送。登记中状态仍创建通知任务，等待期间不消耗发送失败重试次数，登记完成后恢复发送，达到原 TTL 时过期。已接入的消息仍遵循原 TTL，Session 注销或到期阻止新任务，不保证撤回已提交通知。迁移 `00049_pin_push_job_submission` 为任务增加加密提交目标；首次调用推送服务前持久化目标，消息重试保持同一目标、幂等键、正文及绝对到期时间，绑定轮换和 Worker 重启均不改变已提交的请求。`sent` 代表推送服务已接入，不是手机展示回执。

## 私服切换与 Token 转移

推送服务与 MagicChat Server、移动端需配套升级。登记成功时，Server 从推送服务取得 `handoff_token`，作为受限凭据返回给当前认证设备。它只能关闭原应用内的原安装绑定代次，不能发送消息、登记任意 Token、撤销目标 Server 的新绑定或后续代次；有效期为 365 天，例行同步会刷新。保持推送服务密钥环中的旧解密密钥以支持仍有效的凭据。

移动端把凭据和待转移列表保存在 SecureStore，不写入 AsyncStorage 撤销队列。切换 Server 时先持久保存原凭据及原 Server 的待撤销记录；旧 Server 暂不可达时，将凭据提交给新 Server。新 Server 经自己的推送服务授权兑换凭据，推送服务在安装锁内原子注销旧绑定端点并关闭绑定，之后登记新端点。客户端始终只和 Server 通信；长期推送服务凭据不下发。旧 Server 恢复后的清理和转移重放只影响原绑定。

该恢复路径需要设备已取得原登记的转移凭据；升级前尚未取得凭据的设备须先完成一次原 Server 同步。不同 Push 服务或不同应用的凭据不能跨作用域使用；新 Server 会忽略该类凭据，由正常登记确定 Token 是否可用。新登记成功且新凭据已安全保存后，才移除已提交的转移列表；失败保留列表供重试。新增 Push 接口为 `POST /v1/devices/{id}/handoff` 和 `POST /v1/device-handoffs/redeem`，要求 `endpoints:write`，使用 recovery 请求额度，不增加数据库迁移。

临时上游选路不可用返回的 `409 no_eligible_endpoint` 会按退避计划重试，直到原五分钟 TTL 到期；此类等待不受普通发送失败的八次上限限制，也不会改变首次提交目标和幂等内容。

## 指标与验收

Server `/metrics` 提供 `magicchat_mobile_push_*` 队列、失败类别、活跃授权与积压年龄指标。使用 `docker compose --profile monitoring up -d` 启动 Prometheus，规则见 `deploy/monitoring/push-alerts.yml`。私有任务积压时检查服务网络、请求限流、Token 权限和 Worker；上游投递状态在推送服务后台查询。日志不得记录原生设备 Token、服务凭据或路由引用。

部署后分别验证 APNs development/production、FCM 与六家厂商通道：前台抑制、后台通知、点击路由、设备 Token 更新、关闭通知、退出/切换账号、断网撤销恢复。检查重复重试不新增消息且不延长 TTL。电脑活跃暂停偏好、Session 活跃租约和现有推送延迟继续生效。真实厂商凭据及真机验证由部署环境提供。

升级时确认 Server 启动迁移已执行至 49，并为推送业务 Token 增加设备读取权限；更新和清理使用原推送服务凭据。保留数据库及 `credential.key`，保证清理队列在重启后仍可解密。

迁移 `00048_push_registration_recovery` 扩展登记状态并添加清理重试时间/计数；升级前停止旧 Server，配套升级后再启动，避免旧进程处理新的登记中状态。迁移不会删除已有清理任务。
