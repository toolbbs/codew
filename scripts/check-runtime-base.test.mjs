import assert from 'node:assert/strict'
import { readFileSync } from 'node:fs'
import { dirname, resolve } from 'node:path'
import { fileURLToPath } from 'node:url'
import { runInNewContext } from 'node:vm'
import test from 'node:test'

const root = resolve(dirname(fileURLToPath(import.meta.url)), '..')
const source = readFileSync(resolve(root, 'frontend/src/runtimeBase.ts'), 'utf8')
assert.equal(source.split('export function runtimeBasePath()').length, 2)
assert.equal(source.split('import.meta.env.BASE_URL').length, 2)
const executable = source
  .replace('export function runtimeBasePath()', 'function runtimeBasePath()')
  .replace('import.meta.env.BASE_URL', '__viteBase') + '\nruntimeBasePath()'

function baseFor(pathname, viteBase = '/') {
  return runInNewContext(executable, { location: { pathname }, __viteBase: viteBase }, { timeout: 1000 })
}

test('server optimization direct access keeps the root base', () => {
  assert.equal(baseFor('/server-optimization'), '/')
})

test('server optimization trailing slash keeps the root base on reload', () => {
  assert.equal(baseFor('/server-optimization/'), '/')
})

test('existing root views retain their base', () => {
  for (const path of ['/', '/login', '/dashboard', '/servers', '/nodes', '/relays', '/landing-exits', '/clients', '/logs', '/settings', '/diagnostics', '/updates', '/network-policy', '/nodes/fixture']) {
    assert.equal(baseFor(path), '/', path)
  }
})

test('API and public asset paths retain their root base', () => {
  for (const path of ['/api/health', '/assets/fixture.js', '/sub/fixture', '/s/fixture']) {
    assert.equal(baseFor(path), '/', path)
  }
})

test('prefixed routes retain the installation prefix', () => {
  for (const path of ['/preview', '/preview/', '/preview/login', '/preview/nodes', '/preview/server-optimization', '/preview/server-optimization/', '/preview/api/health']) {
    assert.equal(baseFor(path), '/preview/', path)
  }
})

test('explicit Vite base takes precedence over route inference', () => {
  for (const path of ['/', '/login', '/server-optimization', '/preview/nodes']) {
    assert.equal(baseFor(path, '/configured/'), '/configured/', path)
  }
})

test('explicit Vite base without trailing slash is normalized', () => {
  assert.equal(baseFor('/server-optimization', '/configured'), '/configured/')
})

test('root Vite base still allows installation prefix inference', () => {
  assert.equal(baseFor('/preview/server-optimization', '/'), '/preview/')
})

test('missing or empty Vite base uses existing path inference', () => {
  for (const value of [undefined, '', null]) {
    assert.equal(baseFor('/nodes', value), '/')
    assert.equal(baseFor('/preview/nodes', value), '/preview/')
  }
})
