import { installTestAccountRuntime } from "./auth-runtime-test-helper.ts"
import assert from "node:assert/strict"
import test from "node:test"

import type { ApiFetch } from "@/data/api-client"
import {
  getPushDeliveryStatus,
  getPushPreferences,
  reportPushPresence,
  setPushPreferences,
  registerPrivatePushGrant,
  resolvePrivatePushRoute,
} from "@/notifications/push-private-server-api"
import {
  getPushRetryDelay,
  isPendingPushRouteExpired,
  parsePendingPushRouteQueue,
  parsePushDelegation,
  parsePushInstallation,
  parsePushNotificationData,
  pushSynchronizationShouldRetry,
  shouldRefreshPrivateRegistration,
  shouldRenewPushGrant,
  targetsMatch,
} from "@/notifications/push-types"

test("secure push state retains handoff capabilities and rejects malformed queues", () => {
  const ticket = "mph_" + "h".repeat(43)
  const installation = { installationId: "installation", appVersion: "1", platform: "android", manufacturer: "Google", model: "Pixel", registrations: [{ provider: "fcm", token: "fcm-token" }], handoffTokens: [ticket] }
  assert.deepEqual(parsePushInstallation(installation)?.handoffTokens, [ticket])
  assert.equal(parsePushInstallation({ ...installation, handoffTokens: ["invalid"] }), null)
  assert.equal(parsePushInstallation({ ...installation, handoffTokens: Array(11).fill(ticket) }), null)
  const delegation = parsePushDelegation({ accountId: "account", expiresAt: "2099-01-01T00:00:00Z", grantId: "grant", installationId: "installation", lastSyncedAt: null, platform: "android", status: "registered", target: { id: "server", url: "https://example.com", userId: "user" }, handoffToken: ticket })
  assert.equal(delegation?.handoffToken, ticket)
})

test("legacy delegations require the current private registration version", () => {
  const parsed = parsePushDelegation({
    accountId: "account-1",
    expiresAt: "2099-01-01T00:00:00Z",
    grantId: "grant-1",
    installationId: "installation-1",
    lastSyncedAt: "2026-08-28T00:00:00Z",
    platform: "ios",
    sendToken: "send-token",
    status: "registered",
    target: {
      id: "server-1",
      url: "https://private.example",
      userId: "user-1",
    },
  })
  assert.equal(parsed?.privateRegistrationVersion, 1)
})

test("parses only fixed-template push notification routes", () => {
  assert.deepEqual(
    parsePushNotificationData({
      event: "message.created",
      grant_id: "grant-1",
      route_token: "r".repeat(43),
    }),
    {
      event: "message.created",
      grantId: "grant-1",
      routeToken: "r".repeat(43),
    }
  )
  assert.equal(
    parsePushNotificationData({
      event: "message.created",
      grant_id: "grant-1",
      route_token: "short",
      server_url: "https://attacker.example",
    }),
    null
  )
})

test("pending route storage accepts legacy records and queue records", () => {
  const route = {
    grantId: "grant-1",
    receivedAt: "2026-08-27T00:00:00Z",
    routeToken: "r".repeat(43),
  }
  assert.deepEqual(parsePendingPushRouteQueue(route), [route])
  assert.deepEqual(parsePendingPushRouteQueue([route]), [route])
  assert.equal(parsePendingPushRouteQueue([route, { broken: true }]), null)
})

test("compares complete authenticated push targets", () => {
  const target = {
    id: "server-1",
    url: "https://chat.example/",
    userId: "user-1",
  }
  assert.equal(
    targetsMatch(target, {
      id: "server-1",
      url: "https://chat.example",
      userId: "user-1",
    }),
    true
  )
  assert.equal(targetsMatch(target, { ...target, userId: "user-2" }), false)
})

test("calculates grant renewal, registration refresh, and route expiration", () => {
  const now = Date.parse("2026-08-27T00:00:00Z")
  assert.equal(
    shouldRenewPushGrant("2026-09-02T00:00:00Z", now),
    true
  )
  assert.equal(
    shouldRenewPushGrant("2026-09-10T00:00:00Z", now),
    false
  )
  assert.equal(
    shouldRefreshPrivateRegistration("2026-08-25T00:00:00Z", now),
    true
  )
  assert.equal(getPushRetryDelay(0), 5_000)
  assert.equal(getPushRetryDelay(1), 30_000)
  assert.equal(getPushRetryDelay(99), 300_000)
  assert.equal(pushSynchronizationShouldRetry({ status: 503 }), true)
  assert.equal(
    pushSynchronizationShouldRetry({ code: "push_disabled", status: 503 }),
    false
  )
  assert.equal(
    pushSynchronizationShouldRetry({ code: "push_user_disabled" }),
    false
  )
  assert.equal(
    pushSynchronizationShouldRetry({ code: "grant_limit_reached", status: 429 }),
    false
  )
  assert.equal(
    isPendingPushRouteExpired(
      {
        grantId: "grant-1",
        receivedAt: "2026-08-19T00:00:00Z",
        routeToken: "r".repeat(43),
      },
      now
    ),
    true
  )
})

test("push policy uses the current authenticated server without client-supplied user/session IDs", async () => {
  const requests: { init?: RequestInit; url: string }[] = []
  const target = { id: "policy-server", url: "https://private.example", userId: "policy-user" }
  installTestAccountRuntime(target)
  const fetcher: ApiFetch = async (url, init) => {
    requests.push({ url, init })
    const data = url.endsWith("/presence") ? { foreground: true }
      : url.endsWith("/status") ? { enabled: true, registered: true, foreground: true, desktop_active: false, delay_seconds: 5, last_job: null }
      : { pause_when_desktop_active: init?.method === "PUT" }
    return Response.json({ success: true, data })
  }
  await reportPushPresence(target, "ios", true, { fetcher })
  assert.deepEqual(JSON.parse(String(requests[0].init?.body)), { platform: "ios", foreground: true })
  assert.equal((await getPushPreferences(target, { fetcher })).pause_when_desktop_active, false)
  assert.equal((await setPushPreferences(target, { pause_when_desktop_active: true }, { fetcher })).pause_when_desktop_active, true)
  assert.equal((await getPushDeliveryStatus(target, { fetcher })).delay_seconds, 5)
  assert.ok(requests.every((request) => request.url.startsWith("https://private.example/api/client/push/")))
  assert.ok(requests.every((request) => !String(request.init?.body).includes("user_id") && !String(request.init?.body).includes("session_id")))
})

test("registers grants and resolves routes only through the mapped private server", async () => {
  const requests: { init?: RequestInit; url: string }[] = []
  const target = {
    id: "server-1",
    url: "https://private.example/",
    userId: "user-1",
  }
  const responses = [
    { data: { handoff_token: "mph_" + "b".repeat(43) }, success: true },
    {
      data: {
        conversation_id: "conversation-1",
        message_id: "message-1",
      },
      success: true,
    },
  ]
  const fetcher: ApiFetch = async (url, init) => {
    requests.push({ init, url })
    return Response.json(responses.shift(), { status: 200 })
  }
  installTestAccountRuntime(target)
  const registered = await registerPrivatePushGrant(
    target,
    {
      accountId: "account-1",
      expiresAt: "2026-09-27T00:00:00Z",
      grantId: "grant-1",
      installationId: "installation-1",
      lastSyncedAt: null,
      platform: "ios",
      status: "pending_registration",
      target,
    },
    { clientId: "cloud.baizhi.chat", appVersion: "1", installationId: "installation-1", platform: "ios", manufacturer:"Apple", model:"iPhone", registrations:[{provider:"apns",token:"native-token",environment:"production"}], handoffTokens:["mph_"+"a".repeat(43)] },
    { fetcher }
  )
  const route = await resolvePrivatePushRoute(target, "r".repeat(43), {
    fetcher,
  })

  assert.equal(
    requests[0]?.url,
    "https://private.example/api/client/push/grants"
  )
  assert.equal(requests[0]?.init?.credentials, "omit")
  const registration = JSON.parse(String(requests[0]?.init?.body))
  assert.equal(registration.client_id, "cloud.baizhi.chat")
  assert.deepEqual(registration.handoff_tokens, ["mph_"+"a".repeat(43)])
  assert.equal(registered.handoffToken, "mph_"+"b".repeat(43))
  assert.deepEqual(registration.registrations, [{provider:"apns",token:"native-token",environment:"production"}])
  assert.equal(registration.manufacturer,"Apple")
  assert.equal("provider" in registration,false)
  assert.equal("send_token" in registration, false)
  assert.equal(parsePushNotificationData({ event: "message_created", grant_id: "grant-1", route_token: "r".repeat(43) })?.event, "message.created")
  assert.equal(
    requests[1]?.url,
    "https://private.example/api/client/push/routes/resolve"
  )
  assert.equal(requests[1]?.init?.method, "POST")
  assert.deepEqual(JSON.parse(String(requests[1]?.init?.body)), {
    route_token: "r".repeat(43),
  })
  assert.deepEqual(route, {
    conversationId: "conversation-1",
    messageId: "message-1",
  })
})
