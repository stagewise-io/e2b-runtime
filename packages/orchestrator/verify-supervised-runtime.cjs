// Runs at image build and startup. Never starts the orchestrator or touches host state.
const assert = require('node:assert/strict');
const { createHash } = require('node:crypto');
const { readFileSync, accessSync, constants } = require('node:fs');
const { spawnSync } = require('node:child_process');
const root = '/opt/e2b';
const release = JSON.parse(readFileSync(`${root}/release.json`, 'utf8'));
const plan = JSON.parse(readFileSync(`${root}/runtime-release.json`, 'utf8'));
assert.equal(plan.schemaVersion, 1);
assert.equal(release.schema_version, 1);
assert.equal(release.commit, plan.runtimeCommit);
assert.equal(release.version, plan.runtimeVersion);
assert.equal(release.os, 'linux');
assert.equal(release.arch, 'amd64');
assert.equal(process.platform, 'linux');
assert.equal(process.arch, 'x64');
// Explicit chart values must agree with the image, not override its identity.
for (const [env, key] of [['EXPECTED_RUNTIME_COMMIT', 'runtimeCommit'], ['EXPECTED_RUNTIME_VERSION', 'runtimeVersion'], ['EXPECTED_ASSEMBLY_COMMIT', 'assemblyCommit'], ['CELL_NAME', 'cell']]) {
  if (process.env[env] !== undefined) assert.equal(process.env[env], plan[key], `${env} mismatches the assembled release`);
}
const checksums = readFileSync(`${root}/SHA256SUMS`, 'utf8').trim().split('\n');
assert.equal(checksums.length, 2);
for (const name of ['orchestrator', 'clean-nfs-cache']) {
  const line = checksums.find(value => value.endsWith(`  ${name}`));
  assert.match(line ?? '', new RegExp(`^[a-f0-9]{64}  ${name}$`));
  accessSync(`${root}/${name}`, constants.X_OK);
  const binary = readFileSync(`${root}/${name}`);
  assert.equal(createHash('sha256').update(binary).digest('hex'), line.slice(0, 64));
  assert.equal(binary.subarray(0, 4).toString('hex'), '7f454c46');
  assert.equal(binary[4], 2); // ELF64
  assert.equal(binary[5], 1); // little endian
  assert.equal(binary.readUInt16LE(18), 62); // x86-64
  const result = spawnSync('ldd', [`${root}/${name}`], { encoding: 'utf8' });
  assert.ifError(result.error);
  const libraries = result.stdout + result.stderr;
  assert.doesNotMatch(libraries, /not found/);
  if (result.status !== 0) assert.match(libraries, /not a dynamic executable|statically linked/);
}
assert.match(readFileSync('/etc/ssl/certs/ca-certificates.crt', 'utf8'), /BEGIN CERTIFICATE/);
accessSync('/app/main.cjs', constants.R_OK);
accessSync('/app/vendor/info.proto', constants.R_OK);
process.stdout.write('Assembled runtime identity, checksums and dynamic libraries verified\n');
