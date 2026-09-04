import Link from "next/link";
import { getAllPosts } from "@/lib/posts";

function formatDate(date: string) {
  if (!date) return "";
  return new Date(date).toLocaleDateString("fr-FR", {
    day: "numeric",
    month: "long",
    year: "numeric",
  });
}

export default function HomePage() {
  const posts = getAllPosts();

  return (
    <div className="mx-auto max-w-3xl px-6 py-12">
      <section className="mb-12">
        <h1 className="mb-3 text-3xl font-extrabold tracking-tight text-stone-900 sm:text-4xl">
          La route vers l&rsquo;indépendance financière 🔥
        </h1>
        <p className="max-w-2xl text-stone-600">
          Ember explore le mouvement <strong>FIRE</strong> (Financial
          Independence, Retire Early) : stratégies d&rsquo;épargne,
          investissement long terme, calculs concrets et retours
          d&rsquo;expérience — sans jargon inutile.
        </p>
      </section>

      <section className="space-y-8">
        {posts.map((post) => (
          <article
            key={post.slug}
            className="group rounded-xl border border-ember-100 bg-white p-6 shadow-sm transition hover:border-ember-300 hover:shadow-md"
          >
            <Link href={`/posts/${post.slug}`}>
              <h2 className="mb-1 text-xl font-bold text-stone-900 group-hover:text-ember-700">
                {post.title}
              </h2>
              <div className="mb-3 flex items-center gap-3 text-xs font-medium uppercase tracking-wide text-ember-600">
                <time dateTime={post.date}>{formatDate(post.date)}</time>
                <span aria-hidden>&middot;</span>
                <span>{post.readingTime}</span>
              </div>
              <p className="text-stone-600">{post.excerpt}</p>
              {post.tags?.length > 0 && (
                <ul className="mt-4 flex flex-wrap gap-2">
                  {post.tags.map((tag) => (
                    <li
                      key={tag}
                      className="rounded-full bg-ember-50 px-3 py-1 text-xs font-medium text-ember-700"
                    >
                      {tag}
                    </li>
                  ))}
                </ul>
              )}
            </Link>
          </article>
        ))}
      </section>
    </div>
  );
}
