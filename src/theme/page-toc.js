// Build a right-side "On this page" table of contents from the headings
// in the current page (GitBook-style). Runs on every page navigation.

(function () {
    'use strict';

    function buildToc() {
        var existing = document.getElementById('page-toc');
        if (existing) existing.remove();

        var main = document.querySelector('main');
        if (!main) return;

        var headings = main.querySelectorAll('h2, h3, h4');
        if (headings.length < 2) return;

        var aside = document.createElement('aside');
        aside.id = 'page-toc';
        aside.setAttribute('aria-label', 'On this page');

        var title = document.createElement('div');
        title.className = 'page-toc-title';
        title.textContent = 'On this page';
        aside.appendChild(title);

        // Nested-list tree: <ul><li><a/><ul>…</ul></li></ul>
        var rootList = document.createElement('ul');
        rootList.className = 'page-toc-list';
        aside.appendChild(rootList);

        // currentList[level] is the <ul> a heading of that level appends to.
        var currentList = { 2: rootList, 3: null, 4: null };

        function slug(s) {
            return s
                .toLowerCase()
                .replace(/[^a-z0-9\s-]/g, '')
                .trim()
                .replace(/\s+/g, '-');
        }

        function ensureChildList(li) {
            var ul = li.querySelector(':scope > ul');
            if (ul) return ul;
            ul = document.createElement('ul');
            ul.className = 'page-toc-sublist';
            li.appendChild(ul);
            return ul;
        }

        headings.forEach(function (h) {
            if (!h.id) h.id = slug(h.textContent);
            var level = parseInt(h.tagName.substring(1), 10); // 2, 3, 4

            // Find the parent <ul> for this level. Lazily build a child
            // <ul> on the previous-level <li> when the current bucket is
            // empty.
            var parent = currentList[level];
            if (!parent) {
                for (var p = level - 1; p >= 2; p--) {
                    if (currentList[p] && currentList[p].lastElementChild) {
                        parent = ensureChildList(
                            currentList[p].lastElementChild,
                        );
                        break;
                    }
                }
                if (!parent) parent = rootList;
                currentList[level] = parent;
            }

            var li = document.createElement('li');
            li.className =
                'page-toc-item page-toc-' + h.tagName.toLowerCase();

            var a = document.createElement('a');
            a.href = '#' + h.id;
            a.textContent = h.textContent.replace(/^#+\s*/, '');
            a.dataset.target = h.id;
            li.appendChild(a);
            parent.appendChild(li);

            // Any deeper level resets — children will lazy-create a child <ul>.
            for (var d = level + 1; d <= 4; d++) currentList[d] = null;
        });

        var contentEl = document.getElementById('content') || document.body;
        contentEl.appendChild(aside);

        // Highlight the heading currently in view.
        var links = aside.querySelectorAll('a');
        var byId = {};
        links.forEach(function (a) {
            byId[a.dataset.target] = a;
        });

        var observer = new IntersectionObserver(
            function (entries) {
                entries.forEach(function (entry) {
                    var link = byId[entry.target.id];
                    if (!link) return;
                    if (entry.isIntersecting) {
                        links.forEach(function (l) {
                            l.classList.remove('active');
                        });
                        link.classList.add('active');
                    }
                });
            },
            { rootMargin: '-80px 0px -70% 0px', threshold: 0 },
        );
        headings.forEach(function (h) {
            observer.observe(h);
        });
    }

    if (document.readyState === 'loading') {
        document.addEventListener('DOMContentLoaded', buildToc);
    } else {
        buildToc();
    }
})();
