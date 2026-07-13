import { defineConfig } from 'astro/config';
import starlight from '@astrojs/starlight';

const site = process.env.PUBLIC_SITE_URL ?? 'https://pulsys-io.github.io';
const base = process.env.PUBLIC_BASE_PATH ?? '/pulsys';
const repoUrl = process.env.PUBLIC_REPO_URL ?? 'https://github.com/pulsys-io/pulsys';

export default defineConfig({
  site,
  base,
  trailingSlash: 'always',
  integrations: [
    starlight({
      title: 'Pulsys',
      description: 'Open-source pull-through cache for Hugging Face.',
      editLink: {
        baseUrl: `${repoUrl}/edit/main/docs/`,
      },
      social: [
        {
          icon: 'github',
          label: 'GitHub',
          href: repoUrl,
        },
      ],
      customCss: ['./src/styles/starlight-overrides.css'],
      sidebar: [
        {
          label: 'Guides',
          items: [
            { label: 'Overview', link: '/docs/' },
            { label: 'Architecture', link: '/docs/architecture/' },
            { label: 'Benchmarks', link: '/docs/benchmarks/' },
            { label: 'Internals', link: '/docs/internals/' },
            { label: 'Security', link: '/docs/security/' },
            { label: 'OIDC / SSO', link: '/docs/oidc/' },
          ],
        },
        {
          label: 'Project',
          items: [
            {
              label: 'Development guide',
              link: `${repoUrl}/blob/main/DEVELOPMENT.md`,
              attrs: { target: '_blank', rel: 'noopener noreferrer' },
            },
            {
              label: 'GitHub repository',
              link: repoUrl,
              attrs: { target: '_blank', rel: 'noopener noreferrer' },
            },
            {
              label: 'Report a vulnerability',
              link: `${repoUrl}/blob/main/SECURITY.md`,
              attrs: { target: '_blank', rel: 'noopener noreferrer' },
            },
          ],
        },
      ],
    }),
  ],
});
