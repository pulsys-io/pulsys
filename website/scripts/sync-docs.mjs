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
  'troubleshooting-hf-429.md': {
    title: 'Fix Hugging Face 429 Too Many Requests',
    description:
      'Why huggingface_hub, datasets, and ollama downloads fail with HTTP 429 Client Error: Too Many Requests, and how to stop the Hub rate-limiting your IP or token.',
    head: [
      {
        tag: 'script',
        attrs: { type: 'application/ld+json' },
        content: JSON.stringify({
          '@context': 'https://schema.org',
          '@type': 'FAQPage',
          mainEntity: [
            {
              '@type': 'Question',
              name: 'What does 429 Client Error: Too Many Requests mean on Hugging Face?',
              acceptedAnswer: {
                '@type': 'Answer',
                text: 'The Hub is rate-limiting requests from your IP address or token. It is not a permission error and does not require Pro. It is triggered by too many file-download or metadata (HEAD) requests in a short window, usually from high parallelism or many machines sharing one IP.',
              },
            },
            {
              '@type': 'Question',
              name: 'How do I fix HfHubHTTPError: 429 Too Many Requests?',
              acceptedAnswer: {
                '@type': 'Answer',
                text: 'Set HF_TOKEN so requests use your account quota, upgrade huggingface_hub to 1.2 or later so 429s wait on the RateLimit header and retry, reduce max_workers, and prefetch once into a shared cache then load with local_files_only=True or HF_HUB_OFFLINE=1.',
              },
            },
            {
              '@type': 'Question',
              name: 'Does upgrading to Hugging Face Pro fix 429 errors?',
              acceptedAnswer: {
                '@type': 'Answer',
                text: 'Pro raises the rate-limit ceiling but does not remove it. An aggressive client with many parallel workers or an uncached fleet can still exceed a Pro account and receive 429 responses.',
              },
            },
            {
              '@type': 'Question',
              name: 'How do I stop many machines from re-downloading the same model and hitting 429?',
              acceptedAnswer: {
                '@type': 'Answer',
                text: 'Put a pull-through cache such as Pulsys in front of the Hub and point clients at it with HF_ENDPOINT. The first pull fills a local disk cache and every later pull is served from disk, so N machines re-downloading a model become a single upstream download.',
              },
            },
          ],
        }),
      },
    ],
  },
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

/**
 * Serialize a Starlight `head` array to YAML frontmatter lines.
 * @param {Array<{tag: string, attrs?: Record<string, string>, content?: string}>} head
 */
function headToYaml(head) {
  const lines = ['head:'];
  for (const entry of head) {
    lines.push(`  - tag: ${entry.tag}`);
    if (entry.attrs) {
      lines.push('    attrs:');
      for (const [k, v] of Object.entries(entry.attrs)) {
        lines.push(`      ${k}: ${JSON.stringify(v)}`);
      }
    }
    if (entry.content != null) {
      lines.push(`    content: ${JSON.stringify(entry.content)}`);
    }
  }
  return lines.join('\n');
}

/** @param {string} name @param {string} raw */
function withFrontmatter(name, raw) {
  const meta = DOC_META[name];
  if (!meta) return null;
  const body = rewriteLinks(raw.replace(/^# .+\n+/, (m) => m)); // keep H1 in body
  const headBlock = meta.head ? `\n${headToYaml(meta.head)}` : '';
  return `---
title: ${meta.title}
description: ${JSON.stringify(meta.description)}${headBlock}
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
