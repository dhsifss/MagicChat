import assert from "node:assert/strict"
import { createRequire } from "node:module"
import test from "node:test"
import { parseProviderRegistrations, parsePushInstallation } from "@/notifications/push-types"
const require = createRequire(import.meta.url)
const { configuredChannels, configureManifest } = require("../plugins/with-vendor-push.js")

test("device registration preserves each successful SDK token independently from device brand", () => {
  const registrations = parseProviderRegistrations([
    { provider: "huawei_hms", token: "hms-token" }, { provider: "fcm", token: "fcm-token" },
    { provider: "honor_push", token: "honor-token" },
  ], "android")
  assert.deepEqual(registrations?.map((entry) => entry.provider), ["fcm", "honor_push", "huawei_hms"])
  assert.equal(parsePushInstallation({ installationId: "installation", appVersion: "1", manufacturer: "HONOR", model: "test", platform: "android", registrations })?.registrations.length, 3)
  assert.deepEqual(parseProviderRegistrations([], "android"), [])
  assert.equal(parseProviderRegistrations([{ provider: "huawei_hms", token: "one" }, { provider: "huawei_hms", token: "two" }], "android"), null)
  assert.equal(parseProviderRegistrations([{ provider: "apns", token: "apns-token" }], "android"), null)
  assert.equal(parseProviderRegistrations([{ provider: "fcm", token: "fcm-token", environment: "production" }], "android"), null)
})

test("APNs signing environment remains part of registration metadata", () => {
  assert.equal(parseProviderRegistrations([{ provider: "apns", token: "apns-token" }], "ios"), null)
  assert.deepEqual(parseProviderRegistrations([{ provider: "apns", token: "apns-token", environment: "development" }], "ios"), [{ provider: "apns", token: "apns-token", environment: "development" }])
})

test("native build validates complete SDK groups and merges only enabled callbacks", () => {
  assert.deepEqual(configuredChannels({}), [])
  assert.throws(() => configuredChannels({ PUSH_XIAOMI_APP_ID: "id" }), /Incomplete xiaomi/)
  const env = {
    PUSH_HUAWEI_APP_ID: "huawei-id", PUSH_HUAWEI_AGCONNECT_SERVICES: "/config/agconnect.json", PUSH_HONOR_APP_ID: "honor-id",
    PUSH_XIAOMI_APP_ID: "xiaomi-id", PUSH_XIAOMI_APP_KEY: "xiaomi-key", PUSH_OPPO_APP_KEY: "oppo-key", PUSH_OPPO_APP_SECRET: "oppo-sdk-secret",
    PUSH_VIVO_APP_ID: "vivo-id", PUSH_VIVO_APP_KEY: "vivo-key", PUSH_MEIZU_APP_ID: "meizu-id", PUSH_MEIZU_APP_KEY: "meizu-key",
  }
  const enabled = configuredChannels(env)
  assert.equal(enabled.length, 6)
  const original = { application: [{ $: { "android:name": "ExistingApp" } }] }
  const manifest = configureManifest(structuredClone(original), enabled, "cloud.baizhi.chat", env)
  assert.equal(manifest.application[0].service.length, 4)
  assert.equal(manifest.application[0].receiver.length, 3)
  assert.equal(configureManifest(manifest, enabled, "cloud.baizhi.chat", env).application[0].receiver.length, 3)
  assert.deepEqual(configureManifest(structuredClone(original), [], "cloud.baizhi.chat", {}), original)
  const appId = manifest.application[0]["meta-data"].find((entry: any) => entry.$["android:name"] === "com.huawei.hms.client.appid")
  assert.equal(appId.$["android:value"], "appid=huawei-id")
  const disabled = configureManifest(manifest, [], "cloud.baizhi.chat", {})
  assert.equal(disabled.application[0].receiver.length, 0)
  assert.equal(disabled.application[0].service.length, 0)
})
