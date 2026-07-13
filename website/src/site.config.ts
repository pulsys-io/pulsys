/** Canonical project links. Override at build time for forks / Pages base path. */
export const repoHref =
  import.meta.env.PUBLIC_REPO_URL ?? 'https://github.com/pulsys-io/pulsys';

const base = import.meta.env.BASE_URL;

/** On-site Starlight docs (respects PUBLIC_BASE_PATH, e.g. /pulsys/docs). */
export const docsHref = `${base}docs`.replace(/\/?$/, '/docs');
export const getStartedHref = `${base}docs/benchmarks/`.replace(/\/?$/, '/docs/benchmarks/');

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

/**
 * Marketing copy and demo labels.
 */
export const marketing = {
  categoryBadge: 'Open-source artifact cache for ML',
  heroTitle: 'Stop waiting on model downloads.',
  heroLead:
    'An authenticated pull-through cache for Hugging Face. Pull a model once; every later pull is served from local disk at wire speed — no repeat egress, no GPUs idling on downloads.',
  pageDescription:
    'Open-source pull-through cache for Hugging Face. Authenticated by default. Apache-2.0.',
  integrationName: 'Hugging Face Hub',
  baselineCliLabel: 'Direct download',
  /** CLI shown in hero terminal demos. */
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
