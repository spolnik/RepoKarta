class Repokarta < Formula
  desc "Local-first code search, architecture maps, grounded chat, and Deep Wiki"
  homepage "https://github.com/spolnik/RepoKarta"
  head "https://github.com/spolnik/RepoKarta.git", branch: "main"

  depends_on "go" => :build
  depends_on "node" => :build

  def install
    grammar_tags = %w[
      grammar_subset
      grammar_subset_bash
      grammar_subset_go
      grammar_subset_groovy
      grammar_subset_java
      grammar_subset_javascript
      grammar_subset_kotlin
      grammar_subset_python
      grammar_subset_sql
      grammar_subset_tsx
      grammar_subset_typescript
    ].join(",")

    system "npm", "--prefix", "web", "ci"
    system "npm", "--prefix", "web", "test"
    system "npm", "--prefix", "web", "run", "typecheck"
    system "npm", "--prefix", "web", "run", "build"
    system "go", "build", "-tags", grammar_tags, "-trimpath", *std_go_args(output: bin/"repokarta"), "./cmd/repokarta"

    (share/"repokarta/licenses").install "third_party/zoekt/LICENSE" => "zoekt-Apache-2.0.txt"
    (share/"repokarta/licenses").install Dir["third_party/licenses/*"]
  end

  test do
    assert_match(/\A\d+\.\d+\.\d+(?:-[0-9A-Za-z.-]+)?\n\z/, shell_output("#{bin}/repokarta version"))
    assert_path_exists share/"repokarta/licenses/zoekt-Apache-2.0.txt"
  end
end
