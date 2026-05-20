// Replace the textual menu-title with the Versus brand image.
// The image path is resolved relative to the current page so it works
// at every depth (data-path-to-root is set by mdbook).
(function () {
    'use strict';

    function isIntroductionPage() {
        var path = window.location.pathname;
        return path === '/' || path.endsWith('/index.html') ||
               path.endsWith('/introduction.html');
    }

    function injectBrand() {
        var title = document.querySelector('#menu-bar .menu-title');
        if (!title) return;

        // Hide menu-title on the introduction page to avoid duplicate logo.
        if (isIntroductionPage()) {
            title.style.display = 'none';
            return;
        }

        if (title.querySelector('img.brand-logo-img')) return;

        var pathToRoot =
            (document.documentElement &&
                document.documentElement.dataset.pathToRoot) ||
            '';

        title.textContent = '';

        var a = document.createElement('a');
        a.className = 'brand-logo';
        a.href = pathToRoot || './';
        a.setAttribute('aria-label', 'Home');

        var img = document.createElement('img');
        img.className = 'brand-logo-img';
        img.src = pathToRoot + '/docs/images/versus-640-360-px.svg';
        img.alt = 'Versus';
        a.appendChild(img);

        title.appendChild(a);
    }

    // Merge .left-buttons children into .right-buttons so the top bar
    // has a single button cluster on the right.
    function mergeButtonClusters() {
        var menuBar = document.querySelector('#menu-bar');
        if (!menuBar) return;
        var left = menuBar.querySelector('.left-buttons');
        var right = menuBar.querySelector('.right-buttons');
        if (!left || !right || left.dataset.merged === '1') return;
        while (left.firstChild) {
            right.insertBefore(left.firstChild, right.firstChild);
        }
        left.dataset.merged = '1';
        left.style.display = 'none';
    }

    function init() {
        injectBrand();
        mergeButtonClusters();
    }

    if (document.readyState === 'loading') {
        document.addEventListener('DOMContentLoaded', init);
    } else {
        init();
    }
})();
