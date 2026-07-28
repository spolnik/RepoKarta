export interface WikiArticleStats {
  wordCount: number;
  readingMinutes: number;
  sectionCount: number;
}

export function enhanceWikiArticle(article: HTMLElement): WikiArticleStats;
