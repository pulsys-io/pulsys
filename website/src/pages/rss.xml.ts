import rss from '@astrojs/rss';
import type { APIContext } from 'astro';
import { posts } from '../data/posts';
import { brandName, marketing } from '../site.config';

export function GET(context: APIContext) {
  return rss({
    title: `${brandName} blog`,
    description: marketing.pageDescription,
    site: context.site ?? 'https://pulsys.io',
    items: posts.map((post) => ({
      title: post.title,
      description: post.description,
      link: `/blog/${post.slug}/`,
      pubDate: new Date(`${post.published}T00:00:00Z`),
    })),
    customData: '<language>en</language>',
  });
}
