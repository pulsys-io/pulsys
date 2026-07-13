import type { Metadata, Viewport } from 'next';
import { SessionProvider } from '@/lib/session-context';
import './globals.css';

export const metadata: Metadata = {
  title: 'Pulsys Console',
  description: 'Manage your Pulsys model hub deployment',
};

export const viewport: Viewport = {
  width: 'device-width',
  initialScale: 1,
  themeColor: [
    { media: '(prefers-color-scheme: light)', color: '#ffffff' },
    { media: '(prefers-color-scheme: dark)', color: '#000000' },
  ],
};

export default function RootLayout({ children }: { children: React.ReactNode }) {
  return (
    <html lang="en">
      <body>
        <SessionProvider>{children}</SessionProvider>
      </body>
    </html>
  );
}
