/** Canonical project links. Override at build time for forks / Pages base path. */
export const repoHref =
  import.meta.env.PUBLIC_REPO_URL ?? 'https://github.com/pulsys-io/pulsys';

const base = import.meta.env.BASE_URL;

/** On-site Starlight docs (respects PUBLIC_BASE_PATH, e.g. /pulsys/docs). */
export const docsHref = `${base}docs`.replace(/\/?$/, '/docs');
export const getStartedHref = docsHref;

export const githubEditDocsBase = `${repoHref}/edit/main/docs/`;
export const discussionsHref = `${repoHref}/discussions`;
export const issuesHref = `${repoHref}/issues`;
export const releasesHref = `${repoHref}/releases`;
export const ghcrHref = `${repoHref}/pkgs/container/pulsys`;
/** "owner/name" used by the live GitHub-stars button. */
export const repoSlug = repoHref.replace(/^https?:\/\/github\.com\//, '');

/** Community contact for an OSS project routes to Discussions, not sales. */
export const contactHref = discussionsHref;

/** Public brand metadata used in legal pages and footer copy. */
export const brandName = 'Pulsys';
export const companyLegalName = 'Pulsys';
export const legalEmail = 'privacy@pulsys.io';

/** Landing-page copy. Every line is shipped behavior or a published measurement. */
export const marketing = {
  heroTitle: 'A local cache for Hugging Face.',
  heroLead:
    'Self-hosted, authenticated, and compatible with existing Hugging Face clients.',
  pageDescription:
    'Authenticated Hugging Face pull-through cache. Warm hits from local disk. Published EC2 benchmarks. Apache-2.0.',
  primaryCta: 'Get started',
  secondaryCta: 'GitHub',
  finalCtaHeadline: 'Deploy with Docker Compose or Helm',
  finalPrimaryCta: 'Read the docs',
  finalSecondaryCta: 'Star on GitHub',

  /** Value bridge: names the cost the product removes. */
  valueTitle: 'Repeat downloads are the expensive part.',
  valueLead:
    'CI jobs and training nodes often re-pull the same weights on every run. A pull-through cache collapses that into one upstream fill and local-disk warm hits.',

  proofTitle: 'Measured warm path',
  featuresTitle: 'What changes when you put Pulsys in front',
  usecasesTitle: 'Where the cost shows up',
  archTitle: 'How it fits',
  faqTitle: 'FAQ',

  trustPills: ['Apache-2.0', 'Docker Compose', 'Helm', 'SLSA 3', 'Self-hosted'] as const,

  features: [
    {
      title: 'No client rewrites',
      body: 'Set HF_ENDPOINT to Pulsys. huggingface_hub, transformers, datasets, and the hf CLI keep working.',
    },
    {
      title: 'One upstream fill',
      body: 'The first miss streams from Hugging Face onto local disk. Later requests are served from cache.',
    },
    {
      title: 'Warm path at the kernel',
      body: 'Warm hits use io_uring on Linux 6.1+ and sendfile on macOS. Reference EC2 run and reproduction steps live in the benchmarks doc.',
    },
    {
      title: 'Authenticated by default',
      body: 'Every request needs a pulsys_* API key from the admin console. Client credentials never reach Hugging Face.',
    },
    {
      title: 'Pre-warm before traffic',
      body: 'Queue a repo in the console. A background job fills the cache so the fleet starts warm.',
    },
    {
      title: 'Offline after fill',
      body: 'Strict-offline mode serves cached artifacts with zero upstream egress.',
    },
  ] as const,

  usecases: [
    {
      title: 'CI fleets',
      body: 'The same model on every job should not re-cross the public internet. Point HF_ENDPOINT at Pulsys and the second pull is local.',
    },
    {
      title: 'Training clusters',
      body: 'Idle accelerators waiting on egress are the real price of a cold pull. Warm hits stream from local disk.',
    },
    {
      title: 'Air-gapped networks',
      body: 'Pre-warm where there is connectivity, then serve the isolated network with no upstream dependency.',
    },
  ] as const,

  faq: [
    {
      q: 'Which clients work?',
      a: 'Any client that speaks the Hugging Face Hub wire protocol. Set HF_ENDPOINT to Pulsys and use a pulsys_* API key as HF_TOKEN.',
    },
    {
      q: 'Where do credentials live?',
      a: 'Pulsys holds a read-only Hugging Face token (PULSYS_HF_TOKEN) for cache misses. Clients use Pulsys API keys from the admin UI. See the security doc.',
    },
    {
      q: 'Can instances share EFS or S3?',
      a: 'No. The warm path assumes local disk. Shared filesystems cap throughput below local disk.',
    },
    {
      q: 'How do I pre-warm the cache?',
      a: 'Pull through the proxy once, or queue an import from the admin console before traffic arrives.',
    },
  ] as const,

  integrationName: 'Hugging Face Hub',
  baselineCliLabel: 'Direct download',
  cliBinName: 'hf',
  demoModelId: 'Qwen/Qwen2.5-7B-Instruct',
} as const;

/**
 * Analytics scaffold (opt-in):
 * - Set PUBLIC_PLAUSIBLE_DOMAIN to enable script injection.
 * - Optionally override PUBLIC_PLAUSIBLE_SCRIPT_SRC for self-hosting.
 */
export const plausibleDomain = import.meta.env.PUBLIC_PLAUSIBLE_DOMAIN ?? '';
export const plausibleScriptSrc =
  import.meta.env.PUBLIC_PLAUSIBLE_SCRIPT_SRC ?? 'https://plausible.io/js/script.js';
