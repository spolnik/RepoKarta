import assert from "node:assert/strict";
import test from "node:test";
import { JSDOM } from "jsdom";
import { enhanceWikiArticle } from "./wiki-article.mjs";

test("enhanceWikiArticle creates a readable editorial hierarchy", () => {
  const dom = new JSDOM(`<article>
    <h1>Runtime lifecycle</h1>
    <p>This page explains how requests move through the runtime.</p>
    <h2>Startup</h2>
    <p>The command constructs the application.</p>
    <h3>Dependencies</h3>
    <ul><li>Store</li><li>Server</li></ul>
    <h2>Shutdown</h2>
    <p>The process drains in-flight work.</p>
  </article>`);
  const article = dom.window.document.querySelector("article");

  const stats = enhanceWikiArticle(article);

  assert.equal(stats.sectionCount, 2);
  assert.equal(stats.readingMinutes, 1);
  assert.equal(
    article.querySelector(".wiki-reading-meta")?.textContent,
    "1 min read2 sections"
  );
  assert.equal(
    article.querySelector(".wiki-article-intro p")?.textContent,
    "This page explains how requests move through the runtime."
  );
  const sections = article.querySelectorAll(":scope > .wiki-article-section");
  assert.equal(sections.length, 2);
  assert.equal(sections[0].dataset.section, "01");
  assert.equal(sections[0].querySelector("h2")?.textContent, "Startup");
  assert.equal(sections[0].querySelector("h3")?.textContent, "Dependencies");
  assert.equal(sections[1].dataset.section, "02");
  assert.equal(sections[1].querySelector("h2")?.textContent, "Shutdown");
  assert.equal(article.querySelectorAll("p").length, 3);
});

test("enhanceWikiArticle still annotates a page without section headings", () => {
  const dom = new JSDOM("<article><h1>Glossary</h1><p>One compact definition.</p></article>");
  const article = dom.window.document.querySelector("article");

  const stats = enhanceWikiArticle(article);

  assert.equal(stats.sectionCount, 0);
  assert.equal(article.querySelectorAll(".wiki-article-section").length, 0);
  assert.equal(article.querySelector(".wiki-article-intro p")?.textContent, "One compact definition.");
  assert.equal(article.querySelector(".wiki-reading-meta span:last-child")?.textContent, "0 sections");
});

test("enhanceWikiArticle does not create an empty lead from Markdown whitespace", () => {
  const dom = new JSDOM(`<article>
    <h1>Runtime</h1>
    <h2>Startup</h2>
    <p>The service starts.</p>
  </article>`);
  const article = dom.window.document.querySelector("article");

  enhanceWikiArticle(article);

  assert.equal(article.querySelector(".wiki-article-intro"), null);
  assert.equal(article.querySelectorAll(".wiki-article-section").length, 1);
});
