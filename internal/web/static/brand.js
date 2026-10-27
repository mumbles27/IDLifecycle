// brand.js - product name, logo and accent colour from /branding/brand.json (replaceable by the server's branding
// folder). The page works with the defaults if it cannot be loaded.

import { h } from './ui.js';

export const DEFAULT_PRODUCT = 'IDLifecycle';

let current = { product_name: DEFAULT_PRODUCT, accent: '', custom: false };
let pending = null;

/** Loads brand.json once and applies the accent colour. Never rejects. */
export function loadBrand() {
  if (!pending) {
    pending = fetch('/branding/brand.json', { credentials: 'same-origin', cache: 'no-cache' })
      .then((r) => (r.ok ? r.json() : null))
      .then((b) => {
        if (b && typeof b.product_name === 'string' && b.product_name.trim()) {
          current = {
            product_name: b.product_name.trim().slice(0, 40),
            accent: /^#[0-9a-fA-F]{6}$/.test(b.accent || '') ? b.accent : '',
            custom: Boolean(b.custom),
          };
        }
        if (current.accent) {
          document.documentElement.style.setProperty('--accent', current.accent);
          document.documentElement.style.setProperty('--focus', current.accent);
        }
        return current;
      })
      .catch(() => current);
  }
  return pending;
}

export function productName() {
  return current.product_name || DEFAULT_PRODUCT;
}

/** The logo as an <img> (the product name is its alt text, unless decorative next to the name). */
export function brandLogo(cls, { decorative = false } = {}) {
  return h('img', {
    src: '/branding/logo', class: cls, alt: decorative ? '' : productName(), width: 64, height: 64, 'data-test': 'brand-logo',
  });
}

/** document.title for a page. */
export function pageTitle(label) {
  return label ? `${label} - ${productName()}` : productName();
}
