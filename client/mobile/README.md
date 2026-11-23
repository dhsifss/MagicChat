# MagicChat Mobile

MagicChat 的 Expo / React Native 手机客户端，包含：

- 多服务器管理与原生 Bearer 登录（1 小时访问凭据、90 天闲置过期的轮换刷新凭据；最长 365 天，安全存于 SecureStore）
- 左侧抽屉导航
- 共享页面 Header
- 消息、通讯录、项目三个底部 Tab
- 会话列表、联系人目录、项目列表和文本消息收发
- WebSocket 实时消息、断线增量同步和后台通知
- iOS 原生 APNs Token、Android 厂商 SDK 与 FCM Token，通过当前登录的 Server 登记设备和解析通知路由
- 官方服务器与自定义 HTTP/HTTPS 服务器
- Tamagui 2 默认组件与明暗主题

## 本地开发

```bash
pnpm install
pnpm start
```

也可以直接启动指定平台：

```bash
pnpm android
pnpm ios
pnpm web
```

### iOS 模拟器构建

使用 `pnpm ios`，或使用 `pnpm ios --configuration Release` 验证包含本地 JS Bundle 的版本。

模拟器包也需要保留 Xcode 签名和 `application-identifier` 权限。手动调用 `xcodebuild` 时不要设置 `CODE_SIGNING_ALLOWED=NO`；本地模拟器可使用 `CODE_SIGNING_ALLOWED=YES CODE_SIGN_IDENTITY=- DEVELOPMENT_TEAM=<Apple Team ID>`，由 Xcode 生成模拟器权限。关闭签名会使 SecureStore 的钥匙串访问报 `-34018`，导致验证码已验证成功后仍无法保存会话。

## 检查

```bash
pnpm typecheck
pnpm lint
```

## 视频预览

聊天中的视频仅显示 `background4` 占位和 `background2` 播放按钮，不提前请求视频地址或下载。普通会话和合并聊天记录均点击进入独立播放页：先检查缓存，未命中时显示真实下载进度，完整下载并校验后自动播放。

关闭预览、失焦或进入后台会取消未完成的下载并清理半成品；返回前台需手动重试，不提供后台下载或断点续传。播放中的视频进入后台会暂停，返回后不会自动出声。完整缓存可再次复用，不自动保存到相册；Android/iOS 使用应用文件缓存，网页预览使用当前网页会话的 Blob 内存缓存。

## Android 性能诊断

埋点默认关闭，现有安装包不会自动获得埋点。配置好现有的 Android Release 签名环境变量后，重新构建诊断包：

```bash
EXPO_PUBLIC_MOBILE_PERF=1 ./android/gradlew -p android :app:assembleRelease
```

与已安装包签名不同的 APK 无法直接覆盖安装，切勿为了诊断卸载已有应用。前台运行时每 10 秒通过 Android `ReactNativeJS` 日志输出一条 `[mobile-perf]` 汇总，可用 `adb logcat -s ReactNativeJS:I '*:S'` 查看。`n` 为次数，`sum` 和 `max` 为毫秒数（`commit`、`prepare`、`conversation.local_read_reused` 仅使用 `n`）；`conversation.open_to_ready_ms` 从列表按下到聊天页数据可呈现，不等同于首帧绘制；`conversation.local_read_ms` 与 `conversation.sync_elapsed_ms` 分别包含异步本地读取和网络同步的等待时间；`conversations.sqlite_list_ms`、`conversations.sqlite_upsert_ms` 和 `conversations.sqlite_upsert_queue_ms` 分别统计会话列表 SQL 执行、批量写入执行及等待写队列的时间；`contacts.profiles_read_ms`、`contacts.profiles_merge_ms` 和 `contacts.profiles_merge_queue_ms` 对应用户资料的定向读取、合并写入和写队列等待；`js.lag_ms` 表示 100 毫秒定时器晚于预期至少 50 毫秒，不等同于 JS CPU 时间。只记录汇总，不记录账号、对话 ID 或消息内容。日志和诊断包仅用于本地性能分析，普通构建不设置该变量。

诊断包还会输出 `[mobile-startup]` 阶段日志，区分数据库打开/初始化、预览 SQL、本地预览加载、列表数据提交，以及本地历史预热和网络同步；`atMs` 是计时模块加载后的相对毫秒数，`count`/`loaded` 仅为数量。列表提交表示数据已进入 React 提交阶段，不等同于屏幕首帧。日志不会输出 SQL、账号、对话 ID、消息内容或原始异常；普通构建不启用这些日志。

仅用于性能对照时，可同时设置 `EXPO_PUBLIC_MOBILE_SKIP_HISTORY_WARMUP=1`，跳过启动时逐对话预热 20 条历史消息；此开关必须与 `EXPO_PUBLIC_MOBILE_PERF=1` 一起使用，普通构建不受影响，网络同步和列表预览仍保持原逻辑。

`EXPO_PUBLIC_MOBILE_PROBE_PREVIEW_SQL=1` 与诊断模式一起使用时，会额外同步执行一次相同的只读预览 SQL，只记录耗时和行数；结果不会替换正常的异步读取，用于区分原生查询执行与 JS 回调等待。此探针会增加一次读取，仅用于定位问题，不用于正式性能验收。

`EXPO_PUBLIC_MOBILE_MINIMAL_CONVERSATION_ROWS=1` 与诊断模式一起使用时，将会话行临时替换为纯文本名称和真实消息预览；列表数据、数据库读取和网络同步不变，用于隔离头像、侧滑等复杂行组件的启动开销。该对照视图不提供完整交互，仅用于诊断。

2026-10-14 在 MEIZU_20_Pro 上用同签名 Release 诊断包完成 10 次启动采样，保留原有应用数据。除首次安装后的采样外，后续本地缓存包含 55 个会话，稳定返回 46 个预览，列表模型中 49 行有 40 行获得本地预览：

- 原完整行的预览端到端读取为 743–1112ms；跳过历史预热仍为 899–1001ms，预热不是主要原因。
- 同一只读预览 SQL 同步查询 55 行仅为 1–2ms，但原异步路径仍为 775–855ms，不能把端到端等待当成 SQL 执行时间。
- 只将会话行换成纯文本，保留 FlatList、真实预览、历史预热及网络逻辑后，预览读取降至 69–76ms，空预览到有预览的数据提交间隔降至 145–165ms；恢复完整行后读取再次为 768ms，提交间隔为 955ms。

对照将主要瓶颈定位到复杂会话行的首屏挂载及其异步调度开销，尚未单独区分头像和侧滑组件。提交间隔不是物理屏幕绘制时间。上述为修复前的定位过程。

随后实施首屏调度修复：首批非空会话集合的本地预览读取成功或失败后再挂载完整行；空集合预读不会提前放行，后续会话刷新不重新隐藏列表，切换账号及清缓存保留原隔离和竞态处理；离屏行按每批 4 行、5 屏窗口补齐。没有启用摘要兜底，没有替换头像或侧滑组件，普通构建也使用该修复。

同一手机又进行 3 次优化包冷启动和 1 次旧诊断包反向对照，均保留数据，诊断对照开关全部关闭：

- 优化包预览读取为 **54 / 50 / 60ms**；中间恢复旧诊断包重新测得 **746ms**。
- 从设备启动标记到首次带缓存预览的数据提交为 **1404 / 1400 / 1446ms**；旧诊断包为 **2229ms**。这不是整个应用启动仅需 50ms，也不是物理首帧测量。
- 同轮都读取 55 个会话、47 个本地预览，49 行列表模型中 41 行有缓存预览；优化包首次非空列表提交即带缓存预览，旧包先提交 49 行空预览，977ms 后补齐。
- 最后保留同签名 `2.5.0 / Build 29` 的完整功能优化诊断包，没有卸载或清除应用数据。

iOS 从签名 Entitlement 读取 APNs 环境。Xcode Debug 使用 `development`，Release/TestFlight 使用 `production`；`with-apns-environments` config plugin 会在 prebuild 后保持这组配置。安装、设备绑定和通知路由映射保存在 SecureStore；需要 Development Build 或 TestFlight 真机验证。

Android 按构建配置集成华为、小米、OPPO、vivo、荣耀、魅族原生 SDK，支持同时上报多个成功的注册结果。FCM 通过 `expo-notifications` 获取；构建前设置 `GOOGLE_SERVICES_JSON` 指向 Firebase Android 应用文件，FCM 需要 Google Play services。厂商配置和固定 SDK 版本见 [接入说明](../../docs/mobile-vendor-push.md)。

客户端只向当前已认证的 MagicChat Server 上报平台、制造商、型号与 SDK 注册结果。iOS 的 APNs 登记包含签名环境。Server 登记设备与多个端点后，按安装 ID 请求推送，由推送服务决定最终厂商及降级；长期凭据保存在 Server。

系统通知授权后才启动原生注册，原生 Token 回调会触发同步；设备 Token 集合变化、账号切换或关闭通知会重建/撤销对应绑定。通知只携带随机路由引用，点击后经过当前账号认证解析。真实厂商凭据、包名签名权益和真机投递需部署验收。
