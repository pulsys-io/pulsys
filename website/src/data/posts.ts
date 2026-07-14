/** Blog post metadata shared by the blog index, RSS feed, and JSON-LD. */
export interface Post {
  slug: string;
  title: string;
  description: string;
  /** ISO date (YYYY-MM-DD). */
  published: string;
}

export const posts: Post[] = [
  {
    slug: 'price-of-a-model-pull',
    title: 'The Price of a Model Pull: Benchmarking a Cache at the Syscall Floor',
    description:
      'What repeated model downloads cost a GPU fleet, and how far a pull-through cache can go when the warm path is engineered down to one kernel boundary crossing.',
    published: '2026-07-13',
  },
  {
    slug: 'darwin-warm-path-optimizations',
    title: 'Chasing the Absolute Floor: Zero-Copy, Single-Syscall HTTP on Darwin in Go',
    description:
      'An engineering journey from the default Go net/http serve loop to a perfectly fused, single-syscall cache hit on macOS.',
    published: '2026-05-16',
  },
];
