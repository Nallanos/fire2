import Link from "next/link";

export default function NotFound() {
  return (
    <div className="mx-auto flex max-w-3xl flex-col items-start px-6 py-24">
      <h1 className="mb-3 text-2xl font-bold text-stone-900">
        Page introuvable
      </h1>
      <p className="mb-6 text-stone-600">
        Cet article n&rsquo;existe pas (ou plus).
      </p>
      <Link href="/" className="font-medium text-ember-600 hover:text-ember-700">
        &larr; Retour à l&rsquo;accueil
      </Link>
    </div>
  );
}
