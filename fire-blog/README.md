# Ember — blog FIRE

Un petit blog statique (Next.js 14, App Router) sur le mouvement **FIRE**
(Financial Independence, Retire Early), pensé pour être déployé sans
configuration sur [Vercel](https://vercel.com).

## Stack

- [Next.js 14](https://nextjs.org/) (App Router, React Server Components)
- [Tailwind CSS](https://tailwindcss.com/) pour le style
- Articles écrits en Markdown (`content/posts/*.md`), parsés avec
  `gray-matter` + `remark`, générés en pages statiques (`generateStaticParams`)

## Développer en local

```bash
cd fire-blog
npm install
npm run dev
```

Le site est disponible sur [http://localhost:3000](http://localhost:3000).

## Ajouter un article

Créez un fichier `content/posts/mon-article.md` avec un en-tête frontmatter :

```md
---
title: "Titre de l'article"
date: "2026-02-01"
excerpt: "Résumé en une phrase, affiché sur la page d'accueil."
tags: ["tag1", "tag2"]
---

Contenu de l'article en Markdown.
```

Le site le fera apparaître automatiquement, trié par date décroissante.

## Déployer sur Vercel

Ce projet vit dans un sous-dossier (`fire-blog/`) d'un dépôt monorepo. Lors de
la création du projet sur [vercel.com/new](https://vercel.com/new) :

1. Importez le dépôt GitHub `nallanos/fire2`.
2. Dans **Root Directory**, sélectionnez `fire-blog`.
3. Framework Preset : Vercel détecte automatiquement **Next.js**.
4. Build Command / Output Directory : valeurs par défaut (`next build` / `.next`).
5. Cliquez sur **Deploy**.

Chaque push sur la branche connectée déclenchera un nouveau déploiement.

### Déployer via la CLI Vercel

```bash
npm i -g vercel
cd fire-blog
vercel        # déploiement de preview
vercel --prod # déploiement de production
```

## Structure

```
fire-blog/
├── app/
│   ├── layout.tsx        # layout global (header/footer)
│   ├── page.tsx           # accueil, liste des articles
│   ├── posts/[slug]/      # page d'un article
│   └── about/              # page "À propos"
├── content/posts/          # articles en Markdown
├── lib/posts.ts             # lecture/parsing des articles
└── tailwind.config.ts
```
