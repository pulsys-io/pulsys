#!/usr/bin/env node
/**
 * sync-docs.mjs — copy repo docs/*.md into Starlight content (src/content/docs/docs/).
 * Rewrites in-repo relative links for on-site /docs/ routes and /results/ assets.
 */
import { cpSync, mkdirSync, existsSync, readdirSync, readFileSync, writeFileSync, rmSync } from 'node:fs';
import { join, dirname } from 'node:path';
import { fileURLToPath } from 'node:url';

const __dirname = dirname(fileURLToPath(import.meta.url));
const websiteRoot = join(__dirname, '..');
const repoRoot = join(websiteRoot, '..');
const docsSrc = join(repoRoot, 'docs');
const contentDest = join(websiteRoot, 'src', 'content', 'docs', 'docs');

const DOC_META = {
  'README.md': { title: 'Documentation', description: 'Pulsys documentation index', slug: 'index.md' },
  'benchmarks.md': { title: 'Benchmarks', description: 'Measured numbers and reproduction steps' },
  'architecture.md': { title: 'Architecture', description: 'System components, request flow, and deployment' },
  'internals.md': { title: 'Internals', description: 'Warm-path implementation and OS tuning' },
  'security.md': { title: 'Security', description: 'Credential model, parser hardening, and threat model' },
  'oidc.md': { title: 'OIDC / SSO', description: 'Keycloak, Cognito, and IAM Identity Center setup' },
};

const repoUrl = process.env.PUBLIC_REPO_URL ?? 'https://github.com/pulsys-io/pulsys';

if (!existsSync(docsSrc)) {
  console.error('sync-docs: missing', docsSrc);
  process.exit(1);
}

/** @param {string} body */
function rewriteLinks(body) {
  let out = body;
  // Sibling docs → Starlight routes under /docs/
  for (const name of Object.keys(DOC_META)) {
    if (name === 'README.md') continue;
    const slug = name.replace(/\.md$/, '');
    out = out.replaceAll(`](${name})`, `](/docs/${slug}/)`);
    out = out.replaceAll(`](${name}#`, `](/docs/${slug}/#`);
  }
  // results/ charts (served from public/results after sync-benchmarks)
  out = out.replaceAll('](results/', '](/results/');
  // Repo-root files → GitHub blob links
  out = out.replace(/\]\(\.\.\/([^)]+)\)/g, (_, path) => {
    const clean = path.replace(/#.*$/, '');
    const hash = path.includes('#') ? `#${path.split('#').slice(1).join('#')}` : '';
    return `](${repoUrl}/blob/main/${clean}${hash})`;
  });
  return out;
}

/** @param {string} name @param {string} raw */
function withFrontmatter(name, raw) {
  const meta = DOC_META[name];
  if (!meta) return null;
  const body = rewriteLinks(raw.replace(/^# .+\n+/, (m) => m)); // keep H1 in body
  return `---
title: ${meta.title}
description: ${meta.description}
---

${body.trim()}
`;
}

rmSync(join(websiteRoot, 'src', 'content', 'docs', 'docs'), { recursive: true, force: true });
mkdirSync(contentDest, { recursive: true });

for (const name of readdirSync(docsSrc)) {
  if (!name.endsWith('.md') || name === 'README.md') continue;
  if (!DOC_META[name]) continue;
  const srcPath = join(docsSrc, name);
  const outName = DOC_META[name].slug ?? name;
  const transformed = withFrontmatter(name, readFileSync(srcPath, 'utf8'));
  if (transformed) {
    writeFileSync(join(contentDest, outName), transformed);
  }
}

// Index from docs/README.md
const readmePath = join(docsSrc, 'README.md');
if (existsSync(readmePath)) {
  const indexBody = rewriteLinks(readFileSync(readmePath, 'utf8'));
  writeFileSync(
    join(contentDest, 'index.md'),
    `---
title: Documentation
description: Pulsys documentation index
---

${indexBody.trim()}
`,
  );
}

console.log('sync-docs: wrote src/content/docs/docs/ from repo docs/');
