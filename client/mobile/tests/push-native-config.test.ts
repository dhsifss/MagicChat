import assert from "node:assert/strict"
import { createRequire } from "node:module"
import test from "node:test"

const require = createRequire(import.meta.url)
const configure = require("../app.config.cjs")

test("native notifications preserve application config and use the supplied Firebase app file", () => {
  const original = process.env.GOOGLE_SERVICES_JSON
  try {
    const config = { android: { package: "cloud.baizhi.chat" }, name: "MagicChat" }
    delete process.env.GOOGLE_SERVICES_JSON
    assert.deepEqual(configure({ config }), config)
    process.env.GOOGLE_SERVICES_JSON = "/build/firebase/google-services.json"
    assert.deepEqual(configure({ config }), {
      ...config,
      android: { ...config.android, googleServicesFile: process.env.GOOGLE_SERVICES_JSON },
    })
  } finally {
    if (original === undefined) delete process.env.GOOGLE_SERVICES_JSON
    else process.env.GOOGLE_SERVICES_JSON = original
  }
})
