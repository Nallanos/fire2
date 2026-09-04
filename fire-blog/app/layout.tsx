import type { Metadata } from "next";
import Link from "next/link";
import "./globals.css";

export const metadata: Metadata = {
  metadataBase: new URL("https://example.com"),
  title: {
    default: "Ember — Le blog de la route vers le FIRE",
    template: "%s · Ember",
  },
  description:
    "Ember est un blog sur le mouvement FIRE (Financial Independence, Retire Early) : indépendance financière, épargne, investissement et retraite anticipée.",
};

export default function RootLayout({
  children,
}: {
  children: React.ReactNode;
}) {
  return (
    <html lang="fr">
      <body className="flex min-h-screen flex-col">
        <header className="border-b border-ember-100 bg-white/80 backdrop-blur">
          <div className="mx-auto flex max-w-3xl items-center justify-between px-6 py-5">
            <Link href="/" className="flex items-center gap-2 text-lg font-bold text-ember-700">
              <span aria-hidden>🔥</span>
              Ember
            </Link>
            <nav className="flex gap-6 text-sm font-medium text-stone-600">
              <Link href="/" className="hover:text-ember-600">
                Articles
              </Link>
              <Link href="/about" className="hover:text-ember-600">
                À propos
              </Link>
            </nav>
          </div>
        </header>

        <main className="flex-1">{children}</main>

        <footer className="border-t border-ember-100 bg-white/60">
          <div className="mx-auto max-w-3xl px-6 py-8 text-sm text-stone-500">
            <p>
              Ember · un blog sur le chemin vers l&rsquo;indépendance
              financière. Contenu à but informatif, ne constitue pas un
              conseil financier personnalisé.
            </p>
          </div>
        </footer>
      </body>
    </html>
  );
}
