import { spawnSync } from 'node:child_process'
import { resolve } from 'node:path'

const archive = process.argv[2]
if (!archive) {
  console.error('Usage: node scripts/test-fast-release-package.mjs <release.zip>')
  process.exit(2)
}

const python = process.env.PYTHON3 || 'python3'
const check = String.raw`
import json
import os
import posixpath
import re
import stat
import sys
import zipfile

with zipfile.ZipFile(sys.argv[1]) as archive:
    assert archive.testzip() is None, 'archive CRC failure'
    names = archive.namelist()
    assert len(names) == len(set(names)), 'duplicate archive entries'
    roots = {name.split('/')[0] for name in names}
    assert len(roots) == 1, 'expected one package root'
    root = roots.pop()
    version = archive.read(root + '/VERSION').decode('utf-8').strip()
    assert re.fullmatch(r'[0-9]+(?:\.[0-9]+){2,}-[a-z][a-z0-9]*(?:-[a-z0-9]+)*', version), 'invalid VERSION'
    assert root == 'codew-v' + version, 'unexpected Codew archive root'
    assert os.path.basename(sys.argv[1]) == root + '.zip', 'ZIP filename does not match archive root'
    metadata = json.loads(archive.read(root + '/PACKAGE-MANIFEST.json'))
    assert metadata['latest'] == version and metadata['package'] == root + '.zip', 'invalid archive metadata'
    assert metadata['version'] + '-' + metadata['codename'] == version and 'sha256' not in metadata, 'invalid archive manifest version or SHA256'
    binaries = {'bin/codew-api-linux-amd64', 'bin/codew-agent-linux-amd64'}
    packaged_binaries = {name[len(root)+1:] for name in names if name.startswith(root + '/bin/')}
    assert packaged_binaries == binaries, 'unexpected, legacy or missing binaries'
    for name in names:
        assert not re.search(r'[\x00-\x1f\x7f\\:]', name), 'non-POSIX archive path'
        parts = name.split('/')
        assert len(parts) > 1 and name == posixpath.normpath(name) and not {'', '..', '.'}.intersection(parts), 'unsafe archive path'
        assert parts[1] != 'releases' and not {'.git', 'data', 'node_modules', 'runtime', 'version.json'}.intersection(parts[1:]), 'non-release file'
        assert not any(part == '.env' or (part.startswith('.env.') and part != '.env.example') for part in parts[1:]), 'local configuration in archive'
        info = archive.getinfo(name)
        executable = name.endswith(('.sh', '/scripts/codew', '/scripts/codew-netopt')) or '/bin/' in name
        assert info.create_system == 3, name + ': expected Unix creator metadata'
        assert stat.S_ISREG(info.external_attr >> 16), name + ': expected a regular file'
        assert stat.S_IMODE(info.external_attr >> 16) == (0o755 if executable else 0o644), name + ': incorrect file mode'
    executables = ['install.sh', 'deploy/install.sh', 'deploy/agent-install.sh', 'deploy/uninstall.sh',
                   'scripts/codew', 'scripts/codew-netopt', *sorted(binaries)]
    for path in executables:
        info = archive.getinfo(root + '/' + path)
        assert info.create_system == 3, path + ': executable mode requires Unix creator metadata'
        assert stat.S_IMODE(info.external_attr >> 16) == 0o755, path + ': expected 0755'
    for path in sorted(binaries):
        with archive.open(root + '/' + path) as binary:
            header = binary.read(20)
        assert header[:6] == b'\x7fELF\x02\x01' and header[18:20] == b'\x3e\x00', path + ': expected Linux amd64 ELF'
    html = archive.read(root + '/frontend/dist/index.html').decode('utf-8')
    from html.parser import HTMLParser
    class Assets(HTMLParser):
        def __init__(self):
            super().__init__()
            self.urls = []
            self.kinds = set()
        def handle_starttag(self, tag, attrs):
            attrs = dict(attrs)
            if tag == 'script' and 'src' in attrs:
                self.urls.append(attrs['src'])
                self.kinds.add('script')
            if tag == 'link' and attrs.get('rel') == 'stylesheet':
                self.urls.append(attrs['href'])
                self.kinds.add('stylesheet')
    assets = Assets()
    assets.feed(html)
    assert len(assets.urls) >= 2, 'missing frontend script or stylesheet'
    assert assets.kinds == {'script', 'stylesheet'}, 'both frontend asset kinds required'
    for url in assets.urls:
        assert url.startswith('/assets/'), 'unexpected frontend base path: ' + url
        assert root + '/frontend/dist' + url in names, 'missing referenced frontend asset'
    print(json.dumps({'result': 'PASS', 'entries': len(names), 'executableModes': len(executables),
                      'frontendAssets': assets.urls}))
`
const result = spawnSync(python, ['-I', '-c', check, resolve(archive)], { encoding: 'utf8' })
if (result.stdout) process.stdout.write(result.stdout)
if (result.stderr) process.stderr.write(result.stderr)
if (result.error) {
  console.error(result.error.message)
  process.exit(1)
}
process.exit(result.status ?? 1)
