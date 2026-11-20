(() => {
  'use strict';

  const repository = 'NetCore-Technologies/boxr';
  const releaseURL = `https://api.github.com/repos/${repository}/releases?per_page=10`;
  const toast = document.querySelector('.toast');
  let toastTimeout;

  const announce = (message) => {
    if (!toast) return;
    toast.textContent = message;
    toast.classList.add('show');
    window.clearTimeout(toastTimeout);
    toastTimeout = window.setTimeout(() => toast.classList.remove('show'), 2600);
  };

  const setLink = (id, asset, release) => {
    const link = document.getElementById(id);
    const item = release.assets.find((candidate) => candidate.name === asset);
    if (!link) return;
    if (item) {
      link.href = item.browser_download_url;
      link.setAttribute('aria-label', `Download ${asset}`);
      link.classList.remove('is-unavailable');
    } else {
      link.href = release.html_url;
      link.setAttribute('aria-label', `${asset} is not available; open release page`);
      link.classList.add('is-unavailable');
    }
  };

  const loadLatestRelease = async () => {
    const state = document.getElementById('release-state');
    const version = document.getElementById('release-version');
    try {
      const response = await fetch(releaseURL, {
        headers: { Accept: 'application/vnd.github+json' },
        cache: 'no-cache',
      });
      if (!response.ok) throw new Error(`GitHub returned ${response.status}`);
      const releases = await response.json();
      if (!Array.isArray(releases)) throw new Error('Release list is unavailable');
      const release = releases.find((candidate) => !candidate.draft);
      if (!release || !Array.isArray(release.assets)) throw new Error('No published release assets are available');

      if (state) state.textContent = release.prerelease ? 'Latest pre-release' : 'Latest stable release';
      if (version) version.textContent = release.tag_name || 'Latest';

      const assets = {
        'linux-amd64': `boxr-${release.tag_name}-linux-amd64.tar.gz`,
        'linux-arm64': `boxr-${release.tag_name}-linux-arm64.tar.gz`,
        'deb-amd64': `boxr-${release.tag_name}-amd64.deb`,
        'deb-arm64': `boxr-${release.tag_name}-arm64.deb`,
        'macos-arm64': `boxr-${release.tag_name}-darwin-arm64.tar.gz`,
        'macos-amd64': `boxr-${release.tag_name}-darwin-amd64.tar.gz`,
        'windows-msi': `boxr-${release.tag_name}-windows-amd64.msi`,
        'windows-zip': `boxr-${release.tag_name}-windows-amd64.zip`,
        'checksums-link': `boxr-${release.tag_name}-checksums.txt`,
      };
      Object.entries(assets).forEach(([id, name]) => setLink(id, name, release));
    } catch (error) {
      if (state) state.textContent = 'Release information unavailable';
      if (version) version.textContent = 'See GitHub';
      // Keep the release-page fallback links usable when the API is rate-limited.
      console.warn('Unable to load the latest boxr release:', error);
    }
  };

  document.addEventListener('DOMContentLoaded', () => {
    const year = document.getElementById('year');
    if (year) year.textContent = String(new Date().getFullYear());

    const toggle = document.querySelector('.menu-toggle');
    const nav = document.getElementById('primary-nav');
    if (toggle && nav) {
      toggle.addEventListener('click', () => {
        const expanded = toggle.getAttribute('aria-expanded') === 'true';
        toggle.setAttribute('aria-expanded', String(!expanded));
        toggle.setAttribute('aria-label', expanded ? 'Open navigation' : 'Close navigation');
        nav.classList.toggle('open', !expanded);
      });
      nav.querySelectorAll('a').forEach((link) => link.addEventListener('click', () => {
        toggle.setAttribute('aria-expanded', 'false');
        toggle.setAttribute('aria-label', 'Open navigation');
        nav.classList.remove('open');
      }));
    }

    document.querySelectorAll('[data-copy]').forEach((button) => {
      button.addEventListener('click', async () => {
        const command = button.getAttribute('data-copy') || '';
        try {
          await navigator.clipboard.writeText(command);
          announce('Command copied to clipboard.');
        } catch {
          announce('Clipboard access is unavailable in this browser.');
        }
      });
    });

    void loadLatestRelease();
  });
})();
