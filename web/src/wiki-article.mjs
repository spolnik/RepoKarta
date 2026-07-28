const wordsPerMinute = 220;

/**
 * Turn generated Markdown into an editorial document without changing its
 * semantic content. Existing Wiki pages benefit immediately, while the
 * generated Markdown remains portable and readable outside RepoKarta.
 *
 * @param {HTMLElement} article
 * @returns {{ wordCount: number, readingMinutes: number, sectionCount: number }}
 */
export function enhanceWikiArticle(article) {
  const text = article.textContent?.trim() ?? "";
  const wordCount = text ? text.split(/\s+/u).length : 0;
  const readingMinutes = Math.max(1, Math.ceil(wordCount / wordsPerMinute));
  const headings = Array.from(article.children)
    .filter((element) => element.tagName === "H2");
  const sectionCount = headings.length;
  const document = article.ownerDocument;
  const title = article.querySelector(":scope > h1");

  const meta = document.createElement("div");
  meta.className = "wiki-reading-meta";
  meta.setAttribute("aria-label", "Page reading information");

  const readingTime = document.createElement("span");
  readingTime.textContent = `${readingMinutes} min read`;
  const sections = document.createElement("span");
  sections.textContent = `${sectionCount} ${sectionCount === 1 ? "section" : "sections"}`;
  meta.append(readingTime, sections);
  if (title) {
    title.after(meta);
  } else {
    article.prepend(meta);
  }

  const firstHeading = headings[0] ?? null;
  const introStart = meta.nextSibling;
  let introNode = introStart;
  let hasIntroContent = false;
  while (introNode && introNode !== firstHeading) {
    if (introNode.textContent?.trim()) {
      hasIntroContent = true;
      break;
    }
    introNode = introNode.nextSibling;
  }
  if (hasIntroContent && introStart && introStart !== firstHeading) {
    const intro = document.createElement("div");
    intro.className = "wiki-article-intro";
    article.insertBefore(intro, firstHeading);
    let node = introStart;
    while (node && node !== intro) {
      const next = node.nextSibling;
      intro.append(node);
      node = next;
    }
  }

  headings.forEach((heading, index) => {
    const nextHeading = headings[index + 1] ?? null;
    const section = document.createElement("section");
    section.className = "wiki-article-section";
    section.dataset.section = String(index + 1).padStart(2, "0");
    article.insertBefore(section, heading);

    let node = /** @type {ChildNode | null} */ (heading);
    while (node && node !== nextHeading) {
      const next = node.nextSibling;
      section.append(node);
      node = next;
    }
  });

  return { wordCount, readingMinutes, sectionCount };
}
