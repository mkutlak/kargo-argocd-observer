# kargo-argocd-observer — docs site

This branch (`gh-pages`) holds the GitHub Pages site served at
<https://kargo-observer.kutlak.cc>. The page sources are plain Markdown rendered by
GitHub Pages' built-in Jekyll (`jekyll-theme-cayman`).

`architecture.md` and `reference.md` are copies of `docs/` on `main` — when those
change, re-copy them here (`git show main:docs/architecture.md > architecture.md`,
same for `reference.md`) and push.

The project itself lives on the
[`main` branch](https://github.com/mkutlak/kargo-argocd-observer).