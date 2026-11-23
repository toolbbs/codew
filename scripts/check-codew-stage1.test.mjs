import assert from 'node:assert/strict'
import { readFileSync, readdirSync } from 'node:fs'
import { dirname, join, resolve } from 'node:path'
import { fileURLToPath } from 'node:url'
import test from 'node:test'

const root = resolve(dirname(fileURLToPath(import.meta.url)), '..')
const text = path => readFileSync(join(root, path), 'utf8')

test('page identity uses codew without changing the version', () => {
  assert.match(text('frontend/index.html'), /<title>codew<\/title>/)
  assert.match(text('frontend/src/App.vue'), /class="brand">codew</)
  assert.match(text('frontend/src/App.vue'), /class="brand-mark">C</)
  assert.match(text('frontend/src/views/Login.vue'), /<h1>codew<\/h1>/)
  assert.match(text('frontend/src/views/Dashboard.vue'), /codew V0\.7\.8/)
  assert.equal(text('VERSION').trim(), '0.7.8-stable-engineering')
})

test('authentication and router use the same isolated storage keys', () => {
  const api = text('frontend/src/api.ts')
  const main = text('frontend/src/main.ts')
  assert.equal((api.match(/'codew_token'/g) || []).length, 3)
  assert.equal((main.match(/'codew_token'/g) || []).length, 2)
  assert.equal((main.match(/'codew_frontend_version'/g) || []).length, 2)
  assert.doesNotMatch(api + main, /zxy_token|zxy_frontend_version/)
  assert.match(text('scripts/check-server-delete-conflict.mjs'), /localStorage\.setItem\('codew_token'/)
})

test('local outlet bookmarks and downloaded YAML use the new namespace', () => {
  assert.match(text('frontend/src/views/Relays.vue'), /'codew_socks5_outlet_bookmarks_v1'/)
  assert.doesNotMatch(text('frontend/src/views/Relays.vue'), /zxy_socks5_outlet_bookmarks_v1/)
  assert.match(text('frontend/src/views/Clients.vue'), /download="codew-clash.yaml"/)
})

test('Go modules and all internal imports agree', () => {
  for (const module of ['backend', 'agent']) {
    assert.match(text(`${module}/go.mod`), new RegExp(`^module codew/${module}\\s`))
    function walk(dir) {
      for (const entry of readdirSync(dir, { withFileTypes: true })) {
        const path = join(dir, entry.name)
        if (entry.isDirectory()) walk(path)
        else if (entry.name.endsWith('.go')) assert.doesNotMatch(readFileSync(path, 'utf8'), /"zxy-panel\/(backend|agent)\//, path)
      }
    }
    walk(join(root, module))
  }
})

test('frontend package metadata retains its existing dependency graph', () => {
  const pkg = JSON.parse(text('frontend/package.json'))
  const lock = JSON.parse(text('frontend/package-lock.json'))
  assert.equal(pkg.name, 'codew-frontend')
  assert.equal(lock.name, pkg.name)
  assert.equal(lock.packages[''].name, pkg.name)
  assert.equal(pkg.version, '0.7.8')
  assert.equal(lock.version, pkg.version)
  assert.equal(lock.packages[''].version, pkg.version)
  assert.deepEqual(pkg.dependencies, lock.packages[''].dependencies)
  assert.deepEqual(pkg.devDependencies, lock.packages[''].devDependencies)
})

test('operational identity uses codew while protocol defaults remain unchanged', () => {
  assert.match(text('backend/internal/api/health.go'), /"service": "codew-api"/)
  assert.match(text('agent/cmd/agent/main.go'), /getenv\("CODEW_SERVER_ID"/)
  assert.match(text('frontend/src/share.ts'), /valueOr\(node.path, '\/zxy'\)/)
  assert.match(text('backend/internal/xray/reality.go'), /zxy00001/)
  assert.match(text('docs/CODEW_RENAME_STAGE1.md'), /尚不可发布/)
})
