import assert from "node:assert/strict"
import { registerHooks } from "node:module"
import test from "node:test"

import { PushNotificationCoordinator } from "@/notifications/push-notification-coordinator"
import { PushCoordinator } from "@/notifications/push-coordinator"
import { hasActiveRemotePushDelegation, setCurrentPushIdentity } from "@/notifications/push-runtime-state"
import type { PushDelegation, PushInstallation, PushProviderRegistration } from "@/notifications/push-types"
import { pushSynchronizationShouldRetry } from "@/notifications/push-types"

const TARGET = { id: "server-1", url: "https://chat.example.com", userId: "user-1" }
const IDENTITY = { accountId: "account-1", generation: 1, target: TARGET }
const DAY = 24 * 60 * 60 * 1000
const fixture = {
  installation: null as PushInstallation | null,
  delegation: null as PushDelegation | null,
  registrations: [] as { delegation: PushDelegation; installation: PushInstallation }[],
  savedDelegations: [] as PushDelegation[],
  revocations: [] as string[],
  pendingRevocations: [] as unknown[],
  native: [] as PushProviderRegistration[],
  fcmToken: "fcm-token",
  fcmFails: false,
  fcmTimeout: false,
  nativeFails: false,
  requiresFCM: false,
  configured: ["fcm"] as PushProviderRegistration["provider"][],
  unsupported: [] as PushProviderRegistration["provider"][],
  failures: 0,
  revokeFails: false,
  handoffResponse: undefined as string | undefined,
  nextID: 0,
  async register(_target: unknown, delegation: PushDelegation, installation: PushInstallation) {
    this.registrations.push(structuredClone({ delegation, installation }))
    if (this.requiresFCM && !installation.registrations.some((r) => r.provider === "fcm")) throw { code: "unsupported_provider", status: 400 }
    if (this.failures-- > 0 || installation.registrations.length === 0) {
      throw { code: "push_service_unavailable", status: 503 }
    }
    return this.handoffResponse ? { handoffToken: this.handoffResponse } : undefined
  },
}
Object.assign(globalThis, { __pushLifecycleFixture: fixture })

// Exercise the real lifecycle and coordinators with only native I/O and storage
// replaced. The hooks are local to this test process.
const stubs: Record<string, string> = {
  "expo-application": `export const applicationId = "cloud.baizhi.chat"; export const nativeApplicationVersion = "1"; export const getIosPushNotificationServiceEnvironmentAsync = async () => "production"`,
  "expo-device": `export const manufacturer = "Google"; export const modelName = "Pixel"`,
  "expo-notifications": `export const getDevicePushTokenAsync = async () => { if(f.fcmFails) throw new Error("FCM temporarily unavailable"); if(f.fcmTimeout) return new Promise(() => {}); return {type: "android", data: f.fcmToken} }`,
  "expo-crypto": `export const randomUUID = () => "replacement-" + (++f.nextID)`,
  "react-native": `export const Platform = {OS: "android"}`,
  "@/data/api-client": `export class ApiRequestError extends Error {}; export const isUnauthorizedError = (e) => e?.status === 401`,
  "@/notifications/vendor-push-registration": `export const getConfiguredPushProviders = () => f.configured; export const getUnsupportedPushProviders = () => f.unsupported; export const readVendorPushRegistrations = async () => { if(f.nativeFails) throw new Error("native unavailable"); return f.native }`,
  "@/notifications/message-notifications": `export const prepareMessageNotifications = async () => true`,
  "@/notifications/push-private-server-api": `
    export const registerPrivatePushGrant = (...args) => f.register(...args)
    export const revokePrivatePushGrant = async (_target, _account, _installation, grant) => { f.revocations.push(grant); if(f.revokeFails) throw new Error("source server offline") }
  `,
  "@/notifications/push-registration-store": `
    export const loadPushDelegation = async () => structuredClone(f.delegation)
    export const savePushDelegation = async (value) => { f.delegation = structuredClone(value); f.savedDelegations.push(structuredClone(value)) }
    export const clearPushDelegation = async () => { f.delegation = null }
    export const loadPushInstallation = async () => structuredClone(f.installation)
    export const savePushInstallation = async (value) => { f.installation = structuredClone(value) }
    export const loadPushReminderState = async () => ({explicitlyDisabled: false})
    export const loadPendingPushRevocations = async () => f.pendingRevocations
    export const enqueuePendingPushRevocation = async (value) => { f.pendingRevocations.push(value) }
    export const replacePendingPushRevocations = async (value) => { f.pendingRevocations = value }
  `,
  "@/notifications/push-diagnostics": `
    export const updatePushRegistrationDiagnostic = () => {}
    export const getPushRegistrationDiagnostic = () => ({stage: "device_token"})
    export const waitForPushDevice = (value, timeout) => f.fcmTimeout && timeout === 5000 ? Promise.race([value, Promise.reject(new Error("token timeout"))]) : value
  `,
}
registerHooks({
  resolve(specifier, context, nextResolve) {
    if (Object.hasOwn(stubs, specifier)) return { shortCircuit: true, url: `push-lifecycle-test:${encodeURIComponent(specifier)}` }
    return nextResolve(specifier, context)
  },
  load(url, context, nextLoad) {
    if (url.startsWith("push-lifecycle-test:")) {
      return { shortCircuit: true, format: "module", source: `const f = globalThis.__pushLifecycleFixture;\n${stubs[decodeURIComponent(url.slice("push-lifecycle-test:".length))]}` }
    }
    return nextLoad(url, context)
  },
})
const { synchronizePushDelegation, queuePushDelegationRevocation, flushPendingPushRevocation, deactivatePushDelegation } = await import("@/notifications/push-lifecycle")

function reset() {
  fixture.installation = {
    clientId: "cloud.baizhi.chat",
    appVersion: "1", manufacturer: "Google", model: "Pixel", installationId: "installation-1",
    platform: "android", registrations: [{ provider: "fcm", token: "fcm-token" }],
  }
  fixture.delegation = {
    accountId: IDENTITY.accountId, target: TARGET, grantId: "grant-1", installationId: "installation-1",
    platform: "android", status: "registered", privateRegistrationVersion: 4,
    expiresAt: new Date(Date.now() + 6 * DAY).toISOString(), lastSyncedAt: new Date().toISOString(),
  }
  fixture.registrations = []
  fixture.savedDelegations = []
  fixture.revocations = []
  fixture.pendingRevocations = []
  fixture.native = []
  fixture.fcmToken = "fcm-token"
  fixture.fcmFails = false
  fixture.fcmTimeout = false
  fixture.nativeFails = false
  fixture.requiresFCM = false
  fixture.configured = ["fcm"]
  fixture.unsupported = []
  fixture.failures = 0
  fixture.revokeFails = false
  fixture.handoffResponse = undefined
  fixture.nextID = 0
  setCurrentPushIdentity(IDENTITY)
}

test("an existing installation reports its native client ID without replacing its grant", async () => {
  reset()
  delete fixture.installation!.clientId
  fixture.delegation!.expiresAt = new Date(Date.now() + 20 * DAY).toISOString()
  assert.equal(await synchronizePushDelegation(IDENTITY), true)
  assert.equal(fixture.registrations.length, 1)
  assert.equal(fixture.registrations[0].installation.clientId, "cloud.baizhi.chat")
  assert.equal(fixture.delegation!.grantId, "grant-1")
  assert.deepEqual(fixture.revocations, [])
})

test("a changed native application identity replaces the binding and excludes cached tokens", async () => {
  reset()
  fixture.installation!.clientId = "cloud.example.previous"
  fixture.installation!.registrations.push({ provider: "xiaomi_push", token: "previous-client-token" })
  fixture.configured = ["fcm", "xiaomi_push"]
  assert.equal(await synchronizePushDelegation(IDENTITY), true)
  assert.deepEqual(fixture.revocations, ["grant-1"])
  assert.equal(fixture.registrations[0].installation.clientId, "cloud.baizhi.chat")
  assert.deepEqual(fixture.registrations[0].installation.registrations, [{ provider: "fcm", token: "fcm-token" }])
})

test("routine renewal preserves the grant used by notifications already in the tray", async () => {
  reset()
  const previousExpiry = Date.parse(fixture.delegation!.expiresAt)
  assert.equal(await synchronizePushDelegation(IDENTITY), true)
  assert.equal(fixture.delegation!.grantId, "grant-1")
  assert.equal(fixture.delegation!.installationId, "installation-1")
  assert.ok(Date.parse(fixture.delegation!.expiresAt) > previousExpiry)
  assert.equal(fixture.registrations.length, 1)
  assert.deepEqual(fixture.revocations, [])
  assert.equal(fixture.nextID, 0)
  assert.equal(fixture.savedDelegations[0].expiresAt, fixture.registrations[0].delegation.expiresAt)

  let queued = 0
  const notifications = new PushNotificationCoordinator({
    clearLastResponse: () => undefined,
    consumeRoute: async () => undefined,
    enqueueRoute: async () => { queued++ },
    isMissingRouteError: () => false,
    loadDelegation: async () => fixture.delegation,
    loadRoutes: async () => [],
    replaceRoutes: async () => undefined,
    resolveRoute: async () => ({ conversationId: "conversation-1", messageId: "message-1" }),
  })
  assert.equal(await notifications.handleResponse({
    identity: IDENTITY, isCurrent: () => true, navigateLocal: () => undefined,
    response: { data: { event: "message_created", grant_id: "grant-1", route_token: "r".repeat(43) }, date: Date.now(), identifier: "tray-notification" },
  }), true)
  assert.equal(queued, 1)
})

test("a multi-provider build completes first FCM registration on a Pixel", async () => {
  reset()
  fixture.installation = fixture.delegation = null
  fixture.configured = ["fcm", "huawei_hms", "honor_push", "xiaomi_push", "oppo_push", "vivo_push", "meizu_push"]
  fixture.unsupported = ["honor_push", "oppo_push", "vivo_push", "meizu_push"]
  const timers: (() => void)[] = []
  const delays: number[] = []
  const coordinator = new PushCoordinator({
    synchronize: synchronizePushDelegation, deactivate: async () => undefined,
    flushPendingRevocation: async () => undefined, getInstallationId: async () => "installation-1",
    queueRevocation: async () => undefined,
  }, {
    setTimer: (callback, delay) => { timers.push(callback); delays.push(delay); return timers.length as unknown as ReturnType<typeof setTimeout> },
    clearTimer: () => undefined, retryDelay: () => 10, refreshDelay: 60_000,
  })
  try {
    coordinator.configure({ enabled: true, identity: IDENTITY })
    await new Promise<void>((resolve) => setImmediate(resolve))
    assert.equal(coordinator.getState(), "registered")
    assert.equal(fixture.delegation!.status, "registered")
    assert.equal(hasActiveRemotePushDelegation(IDENTITY), true)
    assert.deepEqual(fixture.registrations[0].installation.registrations, [{ provider: "fcm", token: "fcm-token" }])
    assert.deepEqual(delays, [60_000])
  } finally { coordinator.dispose() }
})

test("an explicit SDK support rejection removes its cached token without blocking FCM", async () => {
  reset()
  fixture.delegation!.status = "pending_registration"
  fixture.configured = ["fcm", "honor_push"]
  fixture.installation!.registrations.push({ provider: "honor_push", token: "old-honor-token" })
  fixture.unsupported = ["honor_push"]
  assert.equal(await synchronizePushDelegation(IDENTITY), true)
  assert.equal(fixture.delegation!.status, "registered")
  assert.equal(fixture.delegation!.grantId, "grant-1")
  assert.equal(hasActiveRemotePushDelegation(IDENTITY), true)
  assert.deepEqual(fixture.registrations[0].installation.registrations, [{ provider: "fcm", token: "fcm-token" }])
  assert.deepEqual(fixture.revocations, [])
})

test("first vendor registration can complete when FCM has never been usable", async () => {
  reset()
  fixture.installation = fixture.delegation = null
  fixture.configured = ["fcm", "huawei_hms"]
  fixture.native = [{ provider: "huawei_hms", token: "hms-token" }]
  fixture.fcmFails = true
  assert.equal(await synchronizePushDelegation(IDENTITY), true)
  assert.equal(fixture.delegation!.status, "registered")
  assert.equal(hasActiveRemotePushDelegation(IDENTITY), true)
  assert.deepEqual(fixture.registrations[0].installation.registrations, fixture.native)
})

test("a device unsupported by every bundled SDK stops unavailable registration retries", async () => {
  reset()
  fixture.configured = ["honor_push", "oppo_push"]
  fixture.unsupported = [...fixture.configured]
  await assert.rejects(synchronizePushDelegation(IDENTITY), (error: unknown) => {
    assert.equal((error as { code: string }).code, "android_provider_unavailable")
    assert.equal(pushSynchronizationShouldRetry(error), false)
    return true
  })
  assert.equal(hasActiveRemotePushDelegation(IDENTITY), false)
  assert.deepEqual(fixture.registrations, [])
})

test("a failed renewal retries its persisted lease without changing the binding", async () => {
  reset()
  fixture.failures = 1
  await assert.rejects(synchronizePushDelegation(IDENTITY))
  const renewal = structuredClone(fixture.delegation!)
  assert.equal(renewal.status, "pending_registration")
  assert.equal(renewal.grantId, "grant-1")
  assert.equal(await synchronizePushDelegation(IDENTITY), true)
  assert.equal(fixture.registrations[1].delegation.expiresAt, renewal.expiresAt)
  assert.equal(fixture.registrations[1].delegation.grantId, renewal.grantId)
  assert.deepEqual(fixture.revocations, [])
})

test("a token update retries after failure without replacing its grant", async () => {
  reset()
  fixture.delegation!.expiresAt = new Date(Date.now() + 20 * DAY).toISOString()
  fixture.fcmToken = "refreshed-fcm-token"
  fixture.failures = 1
  await assert.rejects(synchronizePushDelegation(IDENTITY))
  assert.equal(fixture.delegation!.status, "pending_registration")
  assert.equal(await synchronizePushDelegation(IDENTITY), true)
  assert.equal(fixture.registrations.length, 2)
  assert.equal(fixture.registrations[1].installation.registrations[0].token, "refreshed-fcm-token")
  assert.equal(fixture.delegation!.grantId, "grant-1")
  assert.deepEqual(fixture.revocations, [])
})

test("empty SDK results keep the binding and recover through the scheduled retry", async () => {
  reset()
  fixture.delegation!.expiresAt = new Date(Date.now() + 20 * DAY).toISOString()
  fixture.fcmToken = ""
  const timers: (() => void)[] = []
  const coordinator = new PushCoordinator({
    synchronize: synchronizePushDelegation,
    deactivate: async () => undefined,
    flushPendingRevocation: async () => undefined,
    getInstallationId: async () => "installation-1",
    queueRevocation: async () => undefined,
  }, {
    setTimer: (callback) => { timers.push(callback); return timers.length as unknown as ReturnType<typeof setTimeout> },
    clearTimer: () => undefined,
    retryDelay: () => 10,
  })
  try {
    coordinator.configure({ enabled: true, identity: IDENTITY })
    await new Promise<void>((resolve) => setImmediate(resolve))
    assert.equal(coordinator.getState(), "temporarily_unavailable")
    assert.equal(timers.length, 1)
    assert.equal(fixture.delegation!.grantId, "grant-1")
    assert.equal(fixture.delegation!.status, "pending_registration")
    assert.deepEqual(fixture.installation!.registrations, [{ provider: "fcm", token: "fcm-token" }])
    assert.deepEqual(fixture.registrations[0].installation.registrations, [{ provider: "fcm", token: "fcm-token" }])
    fixture.fcmToken = "fcm-token"
    timers.shift()!()
    await new Promise<void>((resolve) => setImmediate(resolve))
    assert.equal(coordinator.getState(), "registered")
    assert.equal(fixture.registrations.length, 2)
    assert.equal(fixture.delegation!.grantId, "grant-1")
    assert.deepEqual(fixture.revocations, [])
  } finally {
    coordinator.dispose()
  }
})

test("partial FCM and vendor lookup failures preserve cached routes and schedule recovery", async () => {
  for (const failed of ["fcm", "timeout", "vendor"]) {
    reset()
    fixture.configured = ["fcm", "huawei_hms"]
    fixture.native = [{ provider: "huawei_hms", token: "hms-token" }]
    fixture.installation!.registrations.push(...fixture.native)
    const originalRegistrations = structuredClone(fixture.installation!.registrations)
    fixture.fcmFails = failed === "fcm"
    fixture.fcmTimeout = failed === "timeout"
    fixture.nativeFails = failed === "vendor"
    const timers: (() => void)[] = []
    const coordinator = new PushCoordinator({
      synchronize: synchronizePushDelegation, deactivate: async () => undefined,
      flushPendingRevocation: async () => undefined, getInstallationId: async () => "installation-1",
      queueRevocation: async () => undefined,
    }, {
      setTimer: (callback) => { timers.push(callback); return timers.length as unknown as ReturnType<typeof setTimeout> },
      clearTimer: () => undefined, retryDelay: () => 10,
    })
    try {
      coordinator.configure({ enabled: true, identity: IDENTITY })
      await new Promise<void>((resolve) => setImmediate(resolve))
      assert.equal(coordinator.getState(), "temporarily_unavailable")
      assert.equal(timers.length, 1)
      assert.equal(fixture.delegation!.grantId, "grant-1")
      assert.deepEqual(fixture.registrations[0].installation.registrations, originalRegistrations)
      assert.deepEqual(fixture.revocations, [])
      fixture.fcmFails = fixture.fcmTimeout = fixture.nativeFails = false
      timers.shift()!()
      await new Promise<void>((resolve) => setImmediate(resolve))
      assert.equal(coordinator.getState(), "registered")
      assert.equal(fixture.delegation!.grantId, "grant-1")
    } finally { coordinator.dispose() }
  }
})

test("a partial snapshot without a cached FCM token retries an FCM-only server", async () => {
  reset()
  fixture.configured = ["fcm", "huawei_hms"]
  fixture.native = [{ provider: "huawei_hms", token: "hms-token" }]
  fixture.installation!.registrations = structuredClone(fixture.native)
  fixture.fcmFails = fixture.requiresFCM = true
  await assert.rejects(synchronizePushDelegation(IDENTITY), (error: unknown) => {
    assert.equal((error as { code: string }).code, "push_device_token_unavailable")
    assert.equal(pushSynchronizationShouldRetry(error), true)
    return true
  })
  assert.equal(fixture.delegation!.grantId, "grant-1")
  assert.deepEqual(fixture.revocations, [])
  fixture.fcmFails = false
  assert.equal(await synchronizePushDelegation(IDENTITY), true)
  assert.equal(fixture.delegation!.grantId, "grant-1")
})

test("a provider removed from the build updates registration while retaining the grant", async () => {
  reset()
  fixture.installation!.registrations.push({ provider: "huawei_hms", token: "previous-hms-token" })
  fixture.delegation!.expiresAt = new Date(Date.now() + 20 * DAY).toISOString()
  assert.equal(await synchronizePushDelegation(IDENTITY), true)
  assert.deepEqual(fixture.registrations[0].installation.registrations, [{ provider: "fcm", token: "fcm-token" }])
  assert.equal(fixture.delegation!.grantId, "grant-1")
  assert.deepEqual(fixture.revocations, [])
})

test("a build that removes every push SDK revokes the binding without retrying", async () => {
  reset()
  fixture.configured = []
  await assert.rejects(synchronizePushDelegation(IDENTITY), (error: unknown) => {
    assert.equal((error as { code: string }).code, "android_provider_unavailable")
    assert.equal(pushSynchronizationShouldRetry(error), false)
    return true
  })
  assert.equal(fixture.delegation, null)
  assert.deepEqual(fixture.revocations, ["grant-1"])
  assert.deepEqual(fixture.registrations, [])
})

test("account changes still revoke the old grant and create a fresh binding", async () => {
  reset()
  fixture.delegation!.accountId = "previous-account"
  fixture.delegation!.target = { ...TARGET, userId: "previous-user" }
  assert.equal(await synchronizePushDelegation(IDENTITY), true)
  assert.deepEqual(fixture.revocations, ["grant-1"])
  assert.notEqual(fixture.delegation!.grantId, "grant-1")
  assert.equal(fixture.delegation!.accountId, IDENTITY.accountId)
})

test("confirmed server logout clears local state without reusing the revoked session", async () => {
  reset()
  await queuePushDelegationRevocation(IDENTITY, true)
  assert.equal(fixture.delegation, null)
  assert.equal(fixture.pendingRevocations.length, 1)
  await flushPendingPushRevocation()
  assert.deepEqual(fixture.pendingRevocations, [])
  assert.deepEqual(fixture.revocations, [])
})

test("switching private servers carries a secure handoff while the old server is offline", async () => {
  for (const flow of ["direct", "coordinator_cleanup"]) {
    reset()
    const sourceTicket = "mph_" + "a".repeat(43)
    const destinationTicket = "mph_" + "b".repeat(43)
    fixture.delegation!.handoffToken = sourceTicket
    fixture.revokeFails = true
    fixture.handoffResponse = destinationTicket
    const next = { ...IDENTITY, accountId: "server-b-account", generation: 2, target: { id: "server-b", url: "https://server-b.example.com", userId: "server-b-user" } }
    if (flow === "coordinator_cleanup") await assert.rejects(deactivatePushDelegation(IDENTITY))
    setCurrentPushIdentity(next)
    assert.equal(await synchronizePushDelegation(next), true)
    assert.equal(fixture.delegation!.handoffToken, destinationTicket)
    assert.equal(fixture.delegation!.accountId, next.accountId)
    assert.deepEqual(fixture.registrations[0].installation.handoffTokens, [sourceTicket])
    assert.deepEqual(fixture.installation!.handoffTokens, [])
    assert.equal(hasActiveRemotePushDelegation(next), true)
    assert.equal(JSON.stringify(fixture.pendingRevocations).includes(sourceTicket), false)
  }
})

test("a failed destination registration retains the handoff for retry", async () => {
  reset()
  const ticket = "mph_" + "a".repeat(43)
  fixture.delegation!.handoffToken = ticket
  fixture.revokeFails = true
  fixture.failures = 1
  const next = { ...IDENTITY, accountId: "server-b-account", generation: 2, target: { ...TARGET, id: "server-b", url: "https://server-b.example.com" } }
  setCurrentPushIdentity(next)
  await assert.rejects(synchronizePushDelegation(next))
  assert.deepEqual(fixture.installation!.handoffTokens, [ticket])
  assert.equal(await synchronizePushDelegation(next), true)
  assert.deepEqual(fixture.registrations[1].installation.handoffTokens, [ticket])
})
