# Android 原生厂商推送

客户端通过本地 Expo Module `modules/vendor-push` 直接使用六家厂商 SDK，取得各自原生 Token。SDK 只在配置完整的构建中参与编译；系统通知授权并进入当前账号推送同步后调用注册。品牌和平台作为设备元数据上报，实际取得的 Token 集合用于声明已注册服务；不根据品牌伪造 Token。

| 厂商 | 固定 SDK | 移动端构建配置 | 未上报应用标识时的 Server 固定通道 |
| --- | --- | --- | --- |
| 华为 HMS | com.huawei.hms:push:6.13.0.301 | PUSH_HUAWEI_APP_ID、PUSH_HUAWEI_AGCONNECT_SERVICES | PUSH_HUAWEI_CHANNEL_ID |
| 小米 | com.umeng.umsdk:xiaomi-push:7.12.4 | PUSH_XIAOMI_APP_ID、PUSH_XIAOMI_APP_KEY | PUSH_XIAOMI_CHANNEL_ID |
| OPPO | com.umeng.umsdk:oppo-push:3.7.1 | PUSH_OPPO_APP_KEY、PUSH_OPPO_APP_SECRET | PUSH_OPPO_CHANNEL_ID |
| vivo | com.umeng.umsdk:vivo-push:4.1.3.0 | PUSH_VIVO_APP_ID、PUSH_VIVO_APP_KEY | PUSH_VIVO_CHANNEL_ID |
| 荣耀 | com.hihonor.mcs:push:8.0.12.307 | PUSH_HONOR_APP_ID | PUSH_HONOR_CHANNEL_ID |
| 魅族 | com.meizu.flyme.internet:push-internal:5.0.3 | PUSH_MEIZU_APP_ID、PUSH_MEIZU_APP_KEY | PUSH_MEIZU_CHANNEL_ID |
| FCM | expo-notifications 的 Firebase SDK | GOOGLE_SERVICES_JSON | PUSH_FCM_CHANNEL_ID |

移动端登记时会自动上报当前包名，推送平台根据已发布厂商配置中的包名匹配通道。多客户端的配置及升级顺序见[推送运维说明](mobile-push-operations.md)。

华为/荣耀使用厂商 Maven 仓库；小米/OPPO/vivo 使用 Maven Central 的原厂二进制重打包，依赖中只引入对应厂商 SDK。小米和 vivo AAR 内附原厂 JAR，编译调用已核对其真实 API。SDK 声明在 `android/build.gradle` 固定版本，不动态加载远端代码。

配置分组只要填写一项就必须填写完整，否则 prebuild 或 Gradle 失败。华为 JSON 要与 App ID、包名匹配，prebuild 复制到 Android assets 后由 AGConnectServicesConfig 加载。FCM 的 JSON 由 Expo 配置处理。构建配置含厂商要求随应用分发的客户端 App ID/Key；推送服务长期 Token、厂商服务端 MasterSecret、服务账号私钥只放在后端。

```sh
# 在当前 shell 导出需要的完整配置组，示例变量见 client/mobile/.env.example。
cd client/mobile
pnpm install --frozen-lockfile
pnpm exec expo prebuild --platform android
pnpm android
```

更改 SDK 配置后必须重新 prebuild 和构建，不能只更新 JavaScript。所有厂商控制台需配置正确包名、签名、通知类别/模板和消息权限，Server 通道须发布并完成准入。同一安装可注册多个厂商 Token，注册集合与上游设备上报一致；最终选择由推送服务完成。FCM 在缺少 Google Play services 的设备上失败时，仍可使用成功取得的厂商 Token。

原生回调保存注册结果并通知 JavaScript 同步。SDK 的通知点击回调及首页启动 Intent 只提取 event、随机 grant_id、route_token，经当前账号路由校验后打开会话。部分厂商通知配置以首页打开为协议边界；各厂商系统/SDK 是否将引用传入点击回调，需要逐机型验收，不能以注册成功替代通知点击或送达验证。

验收应覆盖六家原厂设备、旧荣耀使用 HMS 的设备、原厂与 FCM 同时注册、Token 更新、冷启动和后台点击、用户关闭通知、切换账号、离线撤销恢复。推送服务中检查设备厂商/型号、registered_providers、多个关联端点，以及消息最终选中的厂商。登记、通知受理、系统显示与点击路由是不同验收结果。

## 本地验证

已使用六家 SDK 同时启用的构建配置执行 Android prebuild，并通过 `:magicchat-vendor-push:compileDebugKotlin`、`:magicchat-vendor-push:compileDebugJavaWithJavac` 与 `:app:processDebugMainManifest`。环境为 JDK 21、Gradle 9.3.1、Android API 36；配置使用合成 App ID/Key，仅验证编译、注册 API 签名和七个厂商回调组件合并，不调用真实厂商注册服务。

Server 全量测试、推送并发检测、设备选路测试，以及移动端 66 项推送测试、类型检查和相关 lint 通过。设备上报用例验证制造商与注册列表独立、多个 Token 共用一个安装、按安装发送、空 Token 时保留上游路由和逐端点注销；生命周期用例覆盖续期后旧通知点击、续期失败重试、SDK Token 暂时不可用后的自动恢复与账号切换。真机注册、通知投递和点击行为需要对应厂商环境验收。

PostgreSQL 回归验证活跃 Token/同绑定同厂商唯一约束下的端点续期、更新已提交但响应丢失后的恢复、会话登出级联清理、清理失败后重新登录，以及事务回滚不误提交清理任务。

新增 PostgreSQL 恢复回归覆盖端点过期/已关闭绑定的重新登记、登记响应丢失后的直接注销/账号切换/会话退出，以及连续 25 条损坏清理记录后的新任务推进；校验创建端点前本地恢复引用已经提交。

恢复回归还覆盖 FCM/厂商 SDK 单项失败、FCM 超时、构建移除通道和缺少缓存 Token 时的自动重试；同账号的 Token 更新保留 grant ID。上游 PostgreSQL 契约测试使用登记幂等锁和消息请求内容校验，验证 POST 延迟到注销/清理之后执行时仍不会留下活动端点，以及已受理消息在安装轮换与 Worker 重启后的原目标重试。

原生注册表区分 SDK 明确不支持与暂时失败：荣耀、OPPO、vivo 使用 SDK 支持检查，魅族遵循其 register 方法自身的 Flyme 设备准入检查。不支持的通道不参与 Token 等待，也不保留其旧缓存；支持状态在下次原生刷新时重新检查。多 SDK 构建可仅凭 Server 接受的 FCM 或厂商 Token 完成首次登记。此前成功过的通道暂时失败时继续保留缓存并重试。新增生命周期用例验证 Pixel 的 FCM 登记、厂商通道单独成功、旧缓存排除、全部 SDK 不支持及远程推送状态启用；6 项原生回归及修改的四家适配器的真实 SDK 编译通过。

PostgreSQL 回归进一步覆盖 FCM 被 HMS 替换后的等待与通知恢复、保留 FCM 时继续发送、缺少替换通道时保留原路由、首次准入拒绝后的通道切换/原通道恢复/注销/会话退出，以及结果不明请求遭遇后续拒绝时继续保留清理保护。

新增私服切换回归覆盖原 Server 离线、同一 Push 应用/通道的 Token 转移、目标登记失败后的转移重试与安全存储解析。Server 在单连接及生产 40 连接上限下验证 40 个并发登记，检查取消后 session lock 释放，并验证临时选路失败可恢复到原 TTL。转移 API 的跨应用、篡改、权限、旧绑定重放和后续代次隔离使用真实 Push PostgreSQL 集成测试验证。
