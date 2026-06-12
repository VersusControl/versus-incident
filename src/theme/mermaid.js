// mermaid.js — renders ```mermaid fenced code blocks client-side.
//
// The docs build with stock mdBook (no mdbook-mermaid preprocessor), so a
// mermaid fence arrives in the DOM as <pre><code class="language-mermaid">.
// This script swaps each such block for a <div class="mermaid"> and asks
// Mermaid (pinned major version, loaded as an ES module from jsDelivr) to
// render it. No-op when the page has no mermaid blocks.
(function () {
  var codes = document.querySelectorAll("code.language-mermaid");
  if (codes.length === 0) {
    return;
  }

  codes.forEach(function (code) {
    var host = code.closest("pre") || code;
    var div = document.createElement("div");
    div.className = "mermaid";
    // textContent preserves the raw diagram source even if a highlighter
    // wrapped tokens in spans.
    div.textContent = code.textContent;
    host.replaceWith(div);
  });

  var loader = document.createElement("script");
  loader.type = "module";
  loader.textContent = [
    'import mermaid from "https://cdn.jsdelivr.net/npm/mermaid@11/dist/mermaid.esm.min.mjs";',
    'mermaid.initialize({ startOnLoad: false, theme: "neutral", securityLevel: "strict" });',
    'await mermaid.run({ querySelector: ".mermaid" });',
  ].join("\n");
  document.body.appendChild(loader);
})();
