import { requireOptionalNativeModule } from "expo-modules-core"
import type { PushNotificationResponseData } from "@/notifications/push-notification-coordinator"
import type { PushProviderName, PushProviderRegistration } from "@/notifications/push-types"

type VendorPushModule = {
  getRegistrationsAsync(): Promise<PushProviderRegistration[]>
  getConfiguredProviders?(): PushProviderName[] | null
  getUnsupportedProviders?(): PushProviderName[]
  getLastNotificationResponseAsync(): Promise<PushNotificationResponseData | null>
  clearLastNotificationResponse(): void
  addListener(event: "onNotificationResponse", listener: (value: PushNotificationResponseData) => void): { remove(): void }
  addListener(event: "onRegistrationChanged", listener: () => void): { remove(): void }
}
const native = requireOptionalNativeModule<VendorPushModule>("MagicChatVendorPush")

export async function readVendorPushRegistrations() {
  return native ? native.getRegistrationsAsync() : []
}
export function getConfiguredPushProviders() {
  return native?.getConfiguredProviders?.() ?? null
}
export function getUnsupportedPushProviders() {
  return native?.getUnsupportedProviders?.() ?? []
}
export function addVendorPushRegistrationListener(listener: () => void) {
  return native?.addListener("onRegistrationChanged", listener)
}

export function addVendorNotificationResponseListener(listener: (value: PushNotificationResponseData) => void) {
  return native?.addListener("onNotificationResponse", listener)
}
export async function getLastVendorNotificationResponse() {
  return native ? native.getLastNotificationResponseAsync() : null
}
export function clearLastVendorNotificationResponse() { native?.clearLastNotificationResponse() }
