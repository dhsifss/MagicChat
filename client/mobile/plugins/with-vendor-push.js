const fs = require("node:fs")
const path = require("node:path")
const { withAndroidManifest, withProjectBuildGradle, withDangerousMod } = require("expo/config-plugins")

const CHANNELS = {
  huawei: ["PUSH_HUAWEI_APP_ID", "PUSH_HUAWEI_AGCONNECT_SERVICES"],
  honor: ["PUSH_HONOR_APP_ID"],
  xiaomi: ["PUSH_XIAOMI_APP_ID", "PUSH_XIAOMI_APP_KEY"],
  oppo: ["PUSH_OPPO_APP_KEY", "PUSH_OPPO_APP_SECRET"],
  vivo: ["PUSH_VIVO_APP_ID", "PUSH_VIVO_APP_KEY"],
  meizu: ["PUSH_MEIZU_APP_ID", "PUSH_MEIZU_APP_KEY"],
}
function configuredChannels(environment = process.env) {
  return Object.entries(CHANNELS).filter(([name, keys]) => {
    const count = keys.filter((key) => environment[key]?.trim()).length
    if (count && count !== keys.length) throw new Error(`Incomplete ${name} push SDK configuration`)
    return count > 0
  }).map(([name]) => name)
}
function service(name, action) {
  return { $: { "android:name": `cloud.baizhi.push.${name}`, "android:exported": "false" }, "intent-filter": [{ action: [{ $: { "android:name": action } }] }] }
}
function receiver(name, actions, packageName) {
  return { $: { "android:name": `cloud.baizhi.push.${name}`, "android:exported": "true" }, "intent-filter": [{ action: actions.map((action) => ({ $: { "android:name": action } })), category: [{ $: { "android:name": packageName } }] }] }
}
function configureManifest(manifest, enabled, packageName, environment = process.env) {
  const app = manifest.application[0]
  const ownedMetadata = ["com.huawei.hms.client.appid", "push_kit_auto_init_enabled", "com.hihonor.push.app_id", "com.vivo.push.app_id", "com.vivo.push.api_key"]
  for (const kind of ["service", "receiver", "meta-data"]) {
    if (app[kind]) app[kind] = app[kind].filter((entry) => !String(entry.$["android:name"]).startsWith("cloud.baizhi.push.") && !ownedMetadata.includes(entry.$["android:name"]))
  }
  const add = (kind, entry) => {
    app[kind] ??= []
    app[kind] = app[kind].filter((item) => item.$["android:name"] !== entry.$["android:name"])
    app[kind].push(entry)
  }
  const meta = (name, value) => add("meta-data", { $: { "android:name": name, "android:value": value } })
  if (enabled.includes("huawei")) {
    meta("com.huawei.hms.client.appid", `appid=${environment.PUSH_HUAWEI_APP_ID}`)
    meta("push_kit_auto_init_enabled", "false")
    add("service", service("HuaweiPushService", "com.huawei.push.action.MESSAGING_EVENT"))
  }
  if (enabled.includes("honor")) {
    meta("com.hihonor.push.app_id", environment.PUSH_HONOR_APP_ID)
    add("service", service("HonorPushService", "com.hihonor.push.action.MESSAGING_EVENT"))
  }
  if (enabled.includes("xiaomi")) add("receiver", receiver("XiaomiPushReceiver", ["com.xiaomi.mipush.RECEIVE_MESSAGE", "com.xiaomi.mipush.MESSAGE_ARRIVED", "com.xiaomi.mipush.ERROR"], packageName))
  if (enabled.includes("oppo")) {
    for (const [name, permission, actions] of [
      ["OppoCompatibleService", "com.coloros.mcs.permission.SEND_MCS_MESSAGE", ["com.coloros.mcs.action.RECEIVE_MCS_MESSAGE"]],
      ["OppoMessageService", "com.heytap.mcs.permission.SEND_PUSH_MESSAGE", ["com.heytap.mcs.action.RECEIVE_MCS_MESSAGE", "com.heytap.msp.push.RECEIVE_MCS_MESSAGE"]],
    ]) add("service", { $: { "android:name": `cloud.baizhi.push.${name}`, "android:exported": "true", "android:permission": permission }, "intent-filter": [{ action: actions.map((action) => ({ $: { "android:name": action } })) }] })
    manifest["uses-permission"] ??= []
    for (const name of ["com.coloros.mcs.permission.RECIEVE_MCS_MESSAGE", "com.heytap.mcs.permission.RECIEVE_MCS_MESSAGE"]) {
      if (!manifest["uses-permission"].some((entry) => entry.$["android:name"] === name)) manifest["uses-permission"].push({ $: { "android:name": name } })
    }
  }
  if (enabled.includes("vivo")) {
    meta("com.vivo.push.app_id", environment.PUSH_VIVO_APP_ID)
    meta("com.vivo.push.api_key", environment.PUSH_VIVO_APP_KEY)
    add("receiver", receiver("VivoPushReceiver", ["com.vivo.pushclient.action.RECEIVE"], packageName))
  }
  if (enabled.includes("meizu")) add("receiver", receiver("MeizuPushReceiver", ["com.meizu.flyme.push.intent.MESSAGE", "com.meizu.c2dm.intent.REGISTRATION", "com.meizu.c2dm.intent.RECEIVE"], packageName))
  if (enabled.length) {
    manifest.queries ??= [{ package: [] }]
    manifest.queries[0].package ??= []
    const packages = { huawei: ["com.huawei.hwid", "com.huawei.hms"], honor: ["com.hihonor.id", "com.hihonor.push"], xiaomi: ["com.xiaomi.xmsf"], oppo: ["com.heytap.mcs", "com.coloros.mcs"], vivo: ["com.vivo.pushservice"], meizu: ["com.meizu.cloud"] }
    for (const name of enabled.flatMap((provider) => packages[provider])) {
      if (!manifest.queries[0].package.some((entry) => entry.$["android:name"] === name)) manifest.queries[0].package.push({ $: { "android:name": name } })
    }
  }
  return manifest
}
function withVendorPush(config) {
  const enabled = configuredChannels()
  config = withAndroidManifest(config, (mod) => {
    mod.modResults.manifest = configureManifest(mod.modResults.manifest, enabled, config.android.package)
    return mod
  })
  config = withProjectBuildGradle(config, (mod) => {
    const marker = "// MagicChat native push repositories"
    if (!mod.modResults.contents.includes(marker)) mod.modResults.contents += `
${marker}
allprojects {
  repositories {
    maven {
      url 'https://developer.huawei.com/repo/'
      content { includeGroupByRegex 'com[.]huawei([.].*)?' }
    }
    maven {
      url 'https://developer.hihonor.com/repo/'
      content { includeGroupByRegex 'com[.]hihonor([.].*)?' }
    }
  }
}
`
    return mod
  })
  return withDangerousMod(config, ["android", async (mod) => {
    const assets = path.join(mod.modRequest.platformProjectRoot, "app/src/main/assets")
    const target = path.join(assets, "magicchat-agconnect-services.json")
    if (enabled.includes("huawei")) {
      const source = path.resolve(mod.modRequest.projectRoot, process.env.PUSH_HUAWEI_AGCONNECT_SERVICES)
      const contents = fs.readFileSync(source, "utf8")
      const data = JSON.parse(contents)
      if (String(data.client?.app_id) !== process.env.PUSH_HUAWEI_APP_ID.trim() || data.client?.package_name !== config.android.package) throw new Error("Huawei app configuration does not match this application")
      fs.mkdirSync(assets, { recursive: true })
      fs.writeFileSync(target, contents)
    } else fs.rmSync(target, { force: true })
    return mod
  }])
}
module.exports = withVendorPush
module.exports.configuredChannels = configuredChannels
module.exports.configureManifest = configureManifest
