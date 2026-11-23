import * as Application from "expo-application"
import * as Notifications from "expo-notifications"
import * as Device from "expo-device"
import { getConfiguredPushProviders, getUnsupportedPushProviders, readVendorPushRegistrations } from "@/notifications/vendor-push-registration"
import { parseProviderRegistrations } from "@/notifications/push-types"
import { Platform } from "react-native"

import { ApiRequestError, isUnauthorizedError } from "@/data/api-client"
import * as Crypto from "expo-crypto"
import { prepareMessageNotifications } from "@/notifications/message-notifications"
import {
  registerPrivatePushGrant,
  revokePrivatePushGrant,
} from "@/notifications/push-private-server-api"
import {
  clearPushDelegation,
  enqueuePendingPushRevocation,
  loadPendingPushRevocations,
  replacePendingPushRevocations,
  loadPushDelegation,
  loadPushInstallation,
  loadPushReminderState,
  savePushDelegation,
  savePushInstallation,
} from "@/notifications/push-registration-store"
import { isCurrentPushIdentity, setActiveRemotePushTarget } from "@/notifications/push-runtime-state"
import { getPushRegistrationDiagnostic, updatePushRegistrationDiagnostic, waitForPushDevice, type PushRegistrationStage } from "@/notifications/push-diagnostics"
import {
  shouldRefreshPrivateRegistration,
  shouldRenewPushGrant,
  targetsMatch,
  type PushDelegation,
  type PushEnvironment,
  type PushInstallation,
  type PushAccountIdentity,
  type PushPlatform,
  type PushProviderRegistration,
  type PushProviderName,
  type PendingPushRevocation,
} from "@/notifications/push-types"

const PRIVATE_PUSH_REGISTRATION_VERSION = 4

function reportRegistrationStage(identity: PushAccountIdentity, stage: PushRegistrationStage) {
  if (isCurrentPushIdentity(identity)) updatePushRegistrationDiagnostic(identity, stage)
}

export async function synchronizePushDelegation(
  identity: PushAccountIdentity,
  options: { deviceToken?: string } = {}
) {
  reportRegistrationStage(identity, "permission")
  try {
    const registered = await synchronizeRemotePush(identity, options)
    if (isCurrentPushIdentity(identity)) updatePushRegistrationDiagnostic(identity, registered ? "private_server" : "permission", registered ? "registered" : "failed", registered ? null : "permission_denied")
    return registered
  } catch (error) {
    if (isCurrentPushIdentity(identity)) {
      const rawCode = error && typeof error === "object" && "code" in error ? error.code : null
      const code = typeof rawCode === "string" && /^[a-z0-9_.-]{1,120}$/.test(rawCode) ? rawCode : "push_registration_failed"
      updatePushRegistrationDiagnostic(identity, getPushRegistrationDiagnostic().stage, "failed", code)
    }
    throw error
  }
}

export async function getPushInstallationID(identity: PushAccountIdentity) {
  const delegation = await loadPushDelegation()
  return delegation && delegation.accountId === identity.accountId && targetsMatch(delegation.target, identity.target)
    ? delegation.installationId
    : undefined
}

export async function queuePushDelegationRevocation(
  identity: PushAccountIdentity,
  privateRevoked = false
) {
  const delegation = await loadPushDelegation()
  if (!delegation || delegation.accountId !== identity.accountId || !targetsMatch(delegation.target, identity.target)) return
  setActiveRemotePushTarget(null)
  await retainHandoffToken(delegation)
  await enqueuePendingPushRevocation(toPendingRevocation(delegation, privateRevoked))
  await clearPushDelegation()
}

export async function deactivatePushDelegation(
  identity?: PushAccountIdentity
) {
  const delegation = await loadPushDelegation()
  if (!delegation || (identity && (delegation.accountId !== identity.accountId || !targetsMatch(delegation.target, identity.target)))) {
    if (!identity) setActiveRemotePushTarget(null)
    return
  }
  await markAndRevokeDelegation(delegation, await loadPushInstallation())
}

export async function flushPendingPushRevocation() {
  const entries = await loadPendingPushRevocations()
  const installation = await loadPushInstallation()
  for (const entry of entries) {
    await revokeStoredDelegation(entry, installation)
    await replacePendingPushRevocations((await loadPendingPushRevocations()).filter((item) =>
      !(item.accountId === entry.accountId && item.grantId === entry.grantId)))
  }
}

async function synchronizeRemotePush(
  identity: PushAccountIdentity,
  options: { deviceToken?: string }
) {
  const target = identity.target
  if (Platform.OS !== "ios" && Platform.OS !== "android") {
    setActiveRemotePushTarget(null)
    return false
  }

  if ((await loadPushReminderState()).explicitlyDisabled) {
    setActiveRemotePushTarget(null)
    throw new PushLifecycleError(
      "push_user_disabled",
      "用户已关闭手机通知"
    )
  }

  let delegation = await loadPushDelegation()
  if (
    delegation?.status === "registered" &&
    delegation.accountId === identity.accountId && targetsMatch(delegation.target, identity.target) &&
    Date.parse(delegation.expiresAt) > Date.now()
  ) {
    setActiveRemotePushTarget(identity)
  } else {
    setActiveRemotePushTarget(null)
  }

  if (!(await prepareMessageNotifications())) {
    if (delegation && delegation.accountId === identity.accountId && targetsMatch(delegation.target, identity.target)) {
      await markAndRevokeDelegation(
        delegation,
        await loadPushInstallation()
      )
    }
    return false
  }

  reportRegistrationStage(identity, "device_token")
  const device = await waitForPushDevice(readRemotePushDevice(options.deviceToken))
  if (!device) {
    throw new PushLifecycleError(
      "push_environment_unavailable",
      "推送设备信息暂不可用"
    )
  }
  if (device.platform === "android" && device.configuredProviders?.every((provider) => device.unsupportedProviders?.includes(provider))) {
    if (delegation && delegation.accountId === identity.accountId && targetsMatch(delegation.target, target)) {
      await markAndRevokeDelegation(delegation, await loadPushInstallation())
    }
    throw new PushLifecycleError("android_provider_unavailable", "当前设备无可用的安卓推送通道")
  }
  const appVersion = Application.nativeApplicationVersion ?? "unknown"

  let installation = await loadPushInstallation()
  const deviceChanged = !installation || installation.platform !== device.platform
  // Bundled SDKs are optional on a particular device. Recover providers that
  // previously registered, excluding explicit SDK support-check rejections.
  const cachedRegistrations = deviceChanged ? [] : installation?.registrations ?? []
  const expectedProviders = cachedRegistrations.map((r) => r.provider).filter((provider) =>
    (!device.configuredProviders || device.configuredProviders.includes(provider)) &&
    !device.unsupportedProviders?.includes(provider))
  const missingProviders = new Set<PushProviderName>()
  if (device.platform === "android") {
    for (const provider of expectedProviders) {
      if (!device.registrations.some((r) => r.provider === provider)) missingProviders.add(provider)
    }
  }
  const effectiveRegistrations = parseProviderRegistrations([
    ...device.registrations,
    ...cachedRegistrations.filter((r) => missingProviders.has(r.provider)),
  ], device.platform)
  if (!effectiveRegistrations) throw new PushLifecycleError("push_registration_invalid", "设备注册信息无效")
  const registrationsChanged = JSON.stringify(installation?.registrations) !== JSON.stringify(effectiveRegistrations)
  const tokensUnavailable = device.registrations.length === 0 || missingProviders.size > 0
  const metadataChanged = installation?.manufacturer !== device.manufacturer || installation?.model !== device.model
  if (delegation && (deviceChanged || delegation.accountId !== identity.accountId || !targetsMatch(delegation.target, target) ||
      delegation.status === "pending_revocation" || delegation.privateRegistrationVersion !== PRIVATE_PUSH_REGISTRATION_VERSION)) {
    const switchingAccount = delegation.accountId !== identity.accountId || !targetsMatch(delegation.target, target)
    await markAndRevokeDelegation(delegation, installation, switchingAccount)
    delegation = null
    installation = await loadPushInstallation()
  }
  installation = {
    appVersion, manufacturer: device.manufacturer, model: device.model,
    installationId: installation?.installationId ?? Crypto.randomUUID(),
    platform: device.platform,
    registrations: effectiveRegistrations,
    ...(installation?.handoffTokens?.length ? { handoffTokens: installation.handoffTokens } : {}),
  }
  await savePushInstallation(installation)
  if (!delegation) {
    delegation = {
      expiresAt: new Date(Date.now() + 30 * 24 * 60 * 60 * 1000).toISOString(),
      grantId: Crypto.randomUUID(), installationId: installation.installationId,
      lastSyncedAt: null, platform: installation.platform,
      privateRegistrationVersion: PRIVATE_PUSH_REGISTRATION_VERSION,
      status: "pending_registration", accountId: identity.accountId, target,
    }
    await savePushDelegation(delegation)
  }
  if (shouldRenewPushGrant(delegation.expiresAt)) {
    delegation = {
      ...delegation,
      expiresAt: new Date(Date.now() + 30 * 24 * 60 * 60 * 1000).toISOString(),
      lastSyncedAt: null,
      status: "pending_registration",
    }
    await savePushDelegation(delegation)
  }
  if (tokensUnavailable && delegation.status === "registered") {
    delegation = { ...delegation, lastSyncedAt: null, status: "pending_registration" }
    await savePushDelegation(delegation)
  }

  reportRegistrationStage(identity, "private_server")
  if (
    deviceChanged || registrationsChanged || metadataChanged ||
    delegation.status !== "registered" ||
    delegation.privateRegistrationVersion !== PRIVATE_PUSH_REGISTRATION_VERSION ||
    shouldRefreshPrivateRegistration(delegation.lastSyncedAt)
  ) {
    if (delegation.status !== "pending_registration" || delegation.lastSyncedAt !== null) {
      delegation = { ...delegation, lastSyncedAt: null, status: "pending_registration" }
      await savePushDelegation(delegation)
    }
    try {
      const result = await registerPrivatePushGrant(target, delegation, installation)
      if (result?.handoffToken) {
        delegation = { ...delegation, handoffToken: result.handoffToken }
        await savePushDelegation(delegation)
      }
      if (installation.handoffTokens?.length) {
        installation = { ...installation, handoffTokens: [] }
        await savePushInstallation(installation)
      }
    } catch (error) {
      const lookupIncomplete = tokensUnavailable || device.unavailableProviders?.length || device.configuredProviders?.some((provider) =>
        !device.unsupportedProviders?.includes(provider) && !device.registrations.some((r) => r.provider === provider))
      if (lookupIncomplete && error && typeof error === "object" && "code" in error && error.code === "unsupported_provider") {
        throw new PushLifecycleError("push_device_token_unavailable", "推送设备 Token 暂不可用")
      }
      if (privatePushRegistrationIsPermanentlyRejected(error)) {
        await markAndRevokeDelegation(delegation, installation)
      }
      throw error
    }
    delegation = {
      ...delegation,
      lastSyncedAt: tokensUnavailable ? null : new Date().toISOString(),
      privateRegistrationVersion: PRIVATE_PUSH_REGISTRATION_VERSION,
      status: tokensUnavailable ? "pending_registration" : "registered",
    }
    await savePushDelegation(delegation)
  }

  if (tokensUnavailable) throw new PushLifecycleError("push_device_token_unavailable", "推送设备 Token 暂不可用")

  setActiveRemotePushTarget(identity)
  return true
}

async function markAndRevokeDelegation(
  delegation: PushDelegation,
  installation: PushInstallation | null,
  allowOfflineHandoff = false
) {
  setActiveRemotePushTarget(null)
  await retainHandoffToken(delegation)
  const pending = toPendingRevocation(delegation)
  await enqueuePendingPushRevocation(pending)
  await clearPushDelegation()
  try { await revokeStoredDelegation(pending, installation) }
  catch (error) { if (allowOfflineHandoff && delegation.handoffToken) return; throw error }
  await replacePendingPushRevocations((await loadPendingPushRevocations()).filter((item) =>
    !(item.accountId === pending.accountId && item.grantId === pending.grantId)))
}

async function retainHandoffToken(delegation: PushDelegation) {
  if (!delegation.handoffToken) return
  const installation = await loadPushInstallation()
  if (!installation) return
  const tokens = [...new Set([...(installation.handoffTokens ?? []), delegation.handoffToken])]
  if (tokens.length > 10) throw new PushLifecycleError("push_handoff_queue_full", "推送转移暂不可用")
  await savePushInstallation({ ...installation, handoffTokens: tokens })
}

async function revokeStoredDelegation(
  delegation: PendingPushRevocation,
  _installation: PushInstallation | null
) {
  // Successful server logout persists upstream cleanup before deleting the
  // session-bound grant. Its now-invalid credentials need no second request.
  if (!delegation.privateRevoked) {
    try {
      await revokePrivatePushGrant(
        delegation.target,
        delegation.accountId,
        delegation.installationId,
        delegation.grantId
      )
    } catch (error) {
      if (!isUnauthorizedError(error)) throw error
    }
  }
}

function toPendingRevocation(
  delegation: PushDelegation,
  privateRevoked = false
): PendingPushRevocation {
  return {
    accountId: delegation.accountId,
    grantId: delegation.grantId,
    installationId: delegation.installationId,
    privateRevoked,
    queuedAt: new Date().toISOString(),
    target: delegation.target,
  }
}

async function readRemotePushDevice(
  pendingDeviceToken?: string
): Promise<{
  manufacturer: string
  model: string
  platform: PushPlatform
  registrations: PushProviderRegistration[]
  configuredProviders?: PushProviderName[] | null
  unavailableProviders?: PushProviderName[]
  unsupportedProviders?: PushProviderName[]
} | null> {
  if (Platform.OS === "ios") {
    const token = pendingDeviceToken?.trim() || (await readIOSDeviceToken())
    const environment = await readPushEnvironment()
    return token && environment
      ? { manufacturer: Device.manufacturer ?? "Apple", model: Device.modelName ?? "", platform: "ios", registrations: [{ provider: "apns", token, environment }] }
      : null
  }
  if (Platform.OS === "android") {
    const configuredProviders = getConfiguredPushProviders()
    const [native, fcm] = await Promise.all([
      readVendorPushRegistrations().catch(() => []),
      configuredProviders && !configuredProviders.includes("fcm") ? Promise.resolve("") : pendingDeviceToken ? Promise.resolve(pendingDeviceToken) :
        waitForPushDevice(Notifications.getDevicePushTokenAsync(), 5_000)
          .then((value) => typeof value.data === "string" ? value.data : "")
          .catch(() => ""),
    ])
    const registrations = parseProviderRegistrations([
      ...native, ...(fcm ? [{ provider: "fcm", token: fcm }] : []),
    ], "android")
    if (!registrations) throw new PushLifecycleError("push_registration_invalid", "设备注册信息无效")
    return { manufacturer: Device.manufacturer ?? "", model: Device.modelName ?? "", platform: "android", registrations, configuredProviders,
      unsupportedProviders: getUnsupportedPushProviders(),
      unavailableProviders: !fcm && (!configuredProviders || configuredProviders.includes("fcm")) ? ["fcm"] : [] }
  }
  return null
}

async function readIOSDeviceToken() {
  const token = await Notifications.getDevicePushTokenAsync()
  return token.type === "ios" && typeof token.data === "string"
    ? token.data.trim()
    : ""
}

async function readPushEnvironment(): Promise<PushEnvironment | null> {
  const environment =
    await Application.getIosPushNotificationServiceEnvironmentAsync()
  return environment === "development" || environment === "production"
    ? environment
    : null
}

class PushLifecycleError extends Error {
  code: string

  constructor(code: string, message: string) {
    super(message)
    this.name = "PushLifecycleError"
    this.code = code
  }
}

function privatePushRegistrationIsPermanentlyRejected(error: unknown) {
  return (
    error instanceof ApiRequestError &&
    (error.code === "push_disabled" || error.code === "grant_limit_reached")
  )
}
