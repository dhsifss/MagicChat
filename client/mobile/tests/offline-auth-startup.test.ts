import assert from "node:assert/strict"
import { readFileSync } from "node:fs"
import test from "node:test"
import { runInNewContext } from "node:vm"
import { QueryClient } from "@tanstack/react-query"
import ts from "typescript"

import type { ClientUser } from "@/core/models"
import type { AccountRecord } from "@/data/auth/account-store-core"
import { getContactDisplayName } from "@/domain/contacts/contact-display"

const account: AccountRecord = {
  id: "account-a", serverId: "server-a", url: "https://a.example.test", userId: "user-a",
  name: "Alice", lastUsedAt: "2026-11-24T00:00:00.000Z", status: "ready",
}
const keyFor = (target: { id: string; url: string; userId: string }) =>
  ["current-user", target.id, target.url, target.userId]
const target = { id: account.serverId, url: account.url, userId: account.userId }
const key = keyFor(target)
const flush = () => new Promise<void>(resolve => setImmediate(resolve))

// Execute the real provider's mount effect with native services replaced and a
// real QueryClient, so the cold-start ordering and failed refresh are exercised.
async function start(options: { refresh?: () => Promise<ClientUser>; cached?: ClientUser; credentialStatus?: string } = {}) {
  const client = new QueryClient({ defaultOptions: { queries: { retry: false, gcTime: Infinity } } })
  if (options.cached) client.setQueryData(key, options.cached, { updatedAt: 1234 })
  const effects: (() => void)[] = []
  const published: any[] = []
  let hydrated = false
  let refreshes = 0
  let completion: Promise<unknown> | undefined
  const dependencies: Record<string, unknown> = {
    "react": {
      createContext: () => ({ Provider: "AuthContext" }),
      useCallback: (fn: unknown) => fn, useMemo: (fn: () => unknown) => fn(),
      useRef: (value: unknown) => ({ current: value }),
      useState: (value: unknown) => [value, (next: unknown) => { if (value === false) hydrated = next === true }],
      useEffect: (effect: () => void) => effects.push(effect),
    },
    "react/jsx-runtime": { jsx: () => null },
    "@react-native-async-storage/async-storage": {},
    "@tanstack/react-query": { useQueryClient: () => client },
    "@/data/auth/account-migration": { migrateLegacyAccount: async () => undefined },
    "@/data/auth/account-store": {},
    "@/data/auth/account-runtime-instance": {
      accountStore: {
        hydrate: async () => ({ version: 2, accounts: [account], activeAccountId: account.id }),
        getCredential: async () => ({ status: options.credentialStatus ?? "valid" }),
      },
      accountAuthRuntime: { prepare() {}, install: (snapshot: unknown) => published.push(snapshot) },
    },
    "@/features/bootstrap/session-bootstrap": {
      createSessionBootstrapOperations: () => ({}),
      sessionBootstrapCoordinator: {
        invalidate() {},
        start: () => {
          completion = client.fetchQuery({ queryKey: key, staleTime: 0, queryFn: async () => {
            refreshes++
            return options.refresh ? options.refresh() : Promise.reject(new Error("offline"))
          } })
          return completion
        },
      },
    },
    "@/data/auth/credential-store": {}, "@/data/auth/auth-api": {},
    "@/data/auth/account-deactivation-api": {}, "@/data/auth/account-deactivation-failure": {},
    "@/data/auth/sign-out-transaction": {}, "@/data/auth/install-account-transaction": {},
    "@/data/users/current-user-api": {},
    "@/data/query": { queryKeys: { currentUser: keyFor } },
    "@/providers/push-coordinator-provider": { usePushCoordinator: () => ({}) },
  }
  const source = readFileSync(new URL("../src/providers/auth-provider.tsx", import.meta.url), "utf8")
  const code = ts.transpileModule(source, { compilerOptions: { module: ts.ModuleKind.CommonJS, target: ts.ScriptTarget.ES2022, jsx: ts.JsxEmit.ReactJSX } }).outputText
  const exports: Record<string, any> = {}
  runInNewContext(code, { exports, require: (name: string) => {
    assert.ok(name in dependencies, `Unexpected dependency: ${name}`)
    return dependencies[name]
  } })
  exports.AuthProvider({ children: null })
  effects[0]()
  await flush()
  return { client, published, completion, hydrated: () => hydrated, refreshes: () => refreshes }
}

for (const failure of ["offline", "me endpoint unavailable"]) {
  test(`cold start keeps a complete local user after profile refresh fails: ${failure}`, async () => {
    const app = await start({ refresh: async () => { throw new Error(failure) } })
    try {
      await assert.rejects(app.completion!, new RegExp(failure))
      const user = app.client.getQueryData<ClientUser>(key)!
      assert.deepEqual({ ...user }, { avatar: "", createdAt: "", email: "", id: "user-a", lastOnlineAt: null, name: "Alice", nickname: "", phone: "", status: "active" })
      assert.equal(getContactDisplayName(user), "Alice")
      assert.equal(user.phone.trim(), "")
      assert.equal(app.client.getQueryState(key)?.status, "error")
      assert.equal(app.published[0].accountId, account.id)
      assert.equal(app.hydrated(), true)
      assert.equal(app.refreshes(), 1)
      assert.equal(app.client.getQueryData(keyFor({ ...target, userId: "other" })), undefined)
      assert.equal(app.client.getQueryData(keyFor({ ...target, url: "https://other.example.test" })), undefined)
    } finally { app.client.clear() }
  })
}

const fullProfile: ClientUser = {
  avatar: "avatar.png", createdAt: "2025-01-01T00:00:00Z", email: "alice@example.test", id: "user-a",
  lastOnlineAt: null, name: "Alice", nickname: "Ali", phone: "+8612345", status: "active",
}

test("local fallback remains available during refresh and is replaced by the server profile", async () => {
  let resolve!: (user: ClientUser) => void
  const response = new Promise<ClientUser>(done => { resolve = done })
  const app = await start({ refresh: () => response })
  try {
    assert.equal(app.hydrated(), true)
    assert.equal(getContactDisplayName(app.client.getQueryData<ClientUser>(key)!), "Alice")
    assert.equal(app.client.getQueryState(key)?.isInvalidated, true)
    assert.equal(app.client.getQueryState(key)?.dataUpdatedAt, 0)
    resolve(fullProfile)
    await app.completion
    assert.equal(app.client.getQueryData(key), fullProfile)
    assert.equal(getContactDisplayName(app.client.getQueryData<ClientUser>(key)!), "Ali")
  } finally { app.client.clear() }
})

test("restoring an account retains a complete cached profile and its timestamp on refresh failure", async () => {
  const app = await start({ cached: fullProfile })
  try {
    await assert.rejects(app.completion!, /offline/)
    assert.equal(app.client.getQueryData(key), fullProfile)
    assert.equal(app.client.getQueryState(key)?.dataUpdatedAt, 1234)
  } finally { app.client.clear() }
})

test("an invalid local credential does not publish an authenticated account or user fallback", async () => {
  const app = await start({ credentialStatus: "expired" })
  try {
    assert.equal(app.client.getQueryData(key), undefined)
    assert.equal(app.published[0], null)
    assert.equal(app.refreshes(), 0)
    assert.equal(app.hydrated(), true)
  } finally { app.client.clear() }
})
