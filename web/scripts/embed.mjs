import { cp, lstat, mkdir, readdir, realpath, rm, stat } from 'node:fs/promises';
import { fileURLToPath } from 'node:url';
import path from 'node:path';

const webRoot = fileURLToPath(new URL('../', import.meta.url));
const source = path.resolve(webRoot, 'out');
const target = path.resolve(webRoot, '../internal/managementasset/web_static');
// Fail before changing embedded assets if export was not completed.
if (!(await stat(path.join(source, 'index.html'))).isFile()) {
  throw new Error('Run the static Next build before embedding assets.');
}
await mkdir(target, { recursive: true });
if ((await lstat(target)).isSymbolicLink() || await realpath(target) !== target) {
  throw new Error(`Refusing to replace an aliased asset directory: ${target}`);
}
console.log(`Embedding ${source} into ${target}`);
for (const entry of await readdir(target)) {
  if (entry === 'README.md') continue;
  const child = path.resolve(target, entry);
  if (path.dirname(child) !== target) throw new Error('Asset path escaped destination');
  await rm(child, { recursive: true, force: true });
}
await cp(source, target, { recursive: true });
console.log('Embedded static Plus dashboard; compile Go after this step.');
