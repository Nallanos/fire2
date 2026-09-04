export const metadata = {
  title: "À propos",
  description:
    "Pourquoi Ember, un blog sur le mouvement FIRE et l'indépendance financière.",
};

export default function AboutPage() {
  return (
    <div className="mx-auto max-w-3xl px-6 py-12">
      <h1 className="mb-6 text-3xl font-extrabold tracking-tight text-stone-900">
        À propos d&rsquo;Ember
      </h1>

      <div className="space-y-4 text-stone-700">
        <p>
          Ember est un blog consacré au mouvement{" "}
          <strong>FIRE (Financial Independence, Retire Early)</strong>. On y
          parle d&rsquo;épargne, d&rsquo;investissement long terme, de calculs
          concrets pour estimer son &laquo;&nbsp;nombre FIRE&nbsp;&raquo;, et
          des différentes façons d&rsquo;aborder l&rsquo;indépendance
          financière — qu&rsquo;on vise le Lean FIRE, le Fat FIRE, ou une
          version plus flexible comme le Coast ou le Barista FIRE.
        </p>
        <p>
          L&rsquo;objectif n&rsquo;est pas de vendre un mode de vie unique,
          mais de donner des outils et des repères pour que chacun construise
          sa propre trajectoire vers plus de liberté financière.
        </p>
        <p className="text-sm text-stone-500">
          Le contenu de ce blog est fourni à titre informatif et ne
          constitue pas un conseil en investissement personnalisé.
        </p>
      </div>
    </div>
  );
}
